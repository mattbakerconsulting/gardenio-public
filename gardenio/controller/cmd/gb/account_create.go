package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"gardenio/controller/internal/beadsapi"

	"github.com/spf13/cobra"
)

var (
	accountCreateProject    string
	accountCreateProvider   string
	accountCreateCredential string
	accountCreateModel      string
	accountCreateBaseURL    string
	accountCreateLiteLLMKey string
	accountCreateFallback   string
)

var accountCreateCmd = &cobra.Command{
	Use:   "create <name> --project <p> --provider <provider> --credential secret:<name>[#key]",
	Short: "Hand-author a type:account bead wired to a project",
	Long: `Creates a type:account config bead a spawned pod can resolve for API routing.

The bead is labeled project:<p> so the project's pods pick it up via the
account pool (controller resolveRoleAccount/selectAccount). It carries
the provider, credential reference, and optional model/base_url/litellm_key/
fallback the controller bakes into pod env at spawn.

The --credential scheme is validated. Recognized schemes:
  secret:<k8s-secret>[#key]   read a bearer/token from a Kubernetes Secret
  coopmux:<account>           use a coopmux-brokered credential profile
  env:<VARNAME>               read the credential from a pod env var

Note: today the controller only bakes a pod credential (CLAUDE_CODE_OAUTH_TOKEN,
from the Secret's "token" key) for the secret: scheme. coopmux:/env: validate
and are stored, but the spawned pod gets no token from them yet — use secret:
unless you know the account is wired another way.

provider=openai is the Codex ChatGPT-subscription login (ADR 0004 §C): the
credential must be secret:<name>#auth.json, the bead is stamped
billing_type=subscription, and --base-url/--litellm-key are refused (no API key,
no LiteLLM). The pod mounts the derived <name>-session Secret, never <name>.

Bedrock guard (advice ka-0000005094): a Bedrock-routed account (provider or
model bedrock-*) must NOT point --credential at an Anthropic OAuth token secret
(e.g. gardenio-claude-oauth-*). LiteLLM forwards that sk-ant- bearer upstream and
AWS Bedrock 403s it as a malformed SigV4 signature. This command refuses that
combination with an error.`,
	Example: `  gb account create gardenio-codex --project gardenio --provider openai \
    --credential secret:gardenio-codex-auth#auth.json --model <codex-default>
  gb account create litellm-bedrock --project gardenio --provider bedrock \
    --credential secret:litellm-gardenio-key --model bedrock-claude-opus`,
	Args: cobra.ExactArgs(1),
	RunE: runAccountCreate,
}

func runAccountCreate(cmd *cobra.Command, args []string) error {
	name := strings.TrimSpace(args[0])
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	if name == "" {
		return fmt.Errorf("account name (positional arg) is required")
	}
	if accountCreateProject == "" {
		return fmt.Errorf("--project is required (the project this account is wired to)")
	}
	if accountCreateProvider == "" {
		return fmt.Errorf("--provider is required (e.g. anthropic, openai, bedrock)")
	}
	if accountCreateCredential == "" {
		return fmt.Errorf("--credential is required (e.g. secret:<name>, coopmux:<account>, env:<VAR>)")
	}

	// Validate the credential scheme up front — a malformed credential is the
	// most common authoring mistake and produces a pod that silently fails to
	// authenticate.
	cred, err := parseCredential(accountCreateProvider, accountCreateCredential)
	if err != nil {
		return err
	}

	// Bedrock-vs-OAuth footgun guard (advice ka-0000005094). Enforce regardless
	// of credential scheme — the damaging case is a secret: pointing at an
	// Anthropic OAuth token, but reject defensively if the heuristic matches.
	if isBedrockRouted(accountCreateProvider, accountCreateModel) && cred.Scheme == "secret" && looksLikeOAuthSecret(cred.Name) {
		return fmt.Errorf(
			"refusing to wire a Bedrock-routed account (provider=%q model=%q) to OAuth-token secret %q:\n"+
				"  LiteLLM forwards the sk-ant- bearer upstream and AWS Bedrock 403s it as a malformed SigV4 signature.\n"+
				"  Fix: point --credential at a non-OAuth secret (e.g. secret:litellm-gardenio-key) or use a non-bedrock provider.\n"+
				"  See: gb advice show ka-0000005094",
			accountCreateProvider, accountCreateModel, cred.Name)
	}

	if strings.EqualFold(strings.TrimSpace(accountCreateProvider), "openai") &&
		(accountCreateBaseURL != "" || accountCreateLiteLLMKey != "") {
		return fmt.Errorf("provider openai is a Codex subscription login: --base-url and --litellm-key do not apply (no API key, no LiteLLM)")
	}

	// Refuse to create a duplicate account name. Account beads are resolved by
	// title, so two with the same name shadow each other nondeterministically.
	existing, err := daemon.ListAccountBeads(ctx)
	if err != nil {
		return fmt.Errorf("checking for existing accounts: %w", err)
	}
	if _, dup := existing[name]; dup {
		return fmt.Errorf("account %q already exists; pick a different name or edit the existing bead", name)
	}

	fields := map[string]string{
		"provider":   accountCreateProvider,
		"credential": accountCreateCredential,
		"status":     "active",
	}
	if strings.EqualFold(strings.TrimSpace(accountCreateProvider), "openai") {
		fields["billing_type"] = "subscription"
	}
	if accountCreateModel != "" {
		fields["model"] = accountCreateModel
	}
	if accountCreateBaseURL != "" {
		fields["base_url"] = accountCreateBaseURL
	}
	if accountCreateLiteLLMKey != "" {
		fields["litellm_key"] = accountCreateLiteLLMKey
	}
	if accountCreateFallback != "" {
		fields["fallback"] = accountCreateFallback
	}

	fieldsJSON, err := json.Marshal(fields)
	if err != nil {
		return fmt.Errorf("encoding account fields: %w", err)
	}

	id, err := daemon.CreateBead(ctx, beadsapi.CreateBeadRequest{
		Title:     name,
		Type:      "account",
		Kind:      "config",
		Labels:    []string{"project:" + accountCreateProject},
		CreatedBy: actor,
		Fields:    fieldsJSON,
	})
	if err != nil {
		return fmt.Errorf("creating account bead: %w", err)
	}

	if jsonOutput {
		printJSON(map[string]string{"id": id, "name": name, "project": accountCreateProject})
		return nil
	}

	fmt.Fprintf(os.Stdout, "Created account %q (bead %s), wired to project %q\n", name, id, accountCreateProject)
	fmt.Fprintf(os.Stdout, "  provider:   %s\n", accountCreateProvider)
	fmt.Fprintf(os.Stdout, "  credential: %s\n", accountCreateCredential)
	if accountCreateModel != "" {
		fmt.Fprintf(os.Stdout, "  model:      %s\n", accountCreateModel)
	}
	if cred.Scheme == "secret" && cred.Key == codexAuthCredentialKey {
		fmt.Fprintf(os.Stdout,
			"\nThe controller mounts the derived %q Secret (placeholder refresh token) into the codex pod; the\n"+
				"login document itself stays in %q (operator-created; see ADR 0004 §C).\n",
			cred.Name+"-session", cred.Name)
	} else if cred.Scheme == "secret" {
		fmt.Fprintf(os.Stdout,
			"\nThe controller reads the %q key of K8s Secret %q as the pod's CLAUDE_CODE_OAUTH_TOKEN.\n"+
				"Make sure that Secret exists (synced from gardenio/%s).\n",
			"token", cred.Name, cred.Name)
	}
	return nil
}

// parsedCredential is a validated account credential reference.
type parsedCredential struct {
	Scheme string // "secret", "coopmux", or "env"
	Name   string // secret name / coopmux account / env var
	Key    string // optional #key suffix (only meaningful for the secret scheme)
}

// codexAuthCredentialKey is the Secret key holding the Codex ChatGPT-subscription
// login document (ADR 0004 §C): the file `codex login --device-auth` wrote.
const codexAuthCredentialKey = "auth.json"

// parseCredential validates a --credential value against the supported schemes
// the controller understands (applyAccountConfig in cmd/controller/podspec.go):
// secret:<name>[#key], coopmux:<account>, env:<VAR>. Any other scheme — or an
// empty target — is a hard error so a typo never produces a silently
// unauthenticated pod.
//
// For the secret scheme the controller reads a hardcoded "token" key, so a #key
// suffix other than "token" is rejected: accepting it would let an operator
// believe they selected a different property when the controller would ignore
// it. The one exception is provider=openai (the Codex subscription login), whose
// credential is the whole auth.json document: it must be secret:<name>#auth.json
// (the key defaults to auth.json when the suffix is omitted), and no other
// scheme or key is accepted for it.
func parseCredential(provider, cred string) (parsedCredential, error) {
	scheme, target, ok := strings.Cut(cred, ":")
	if !ok || target == "" {
		return parsedCredential{}, fmt.Errorf(
			"malformed --credential %q: expected scheme:target, e.g. secret:<name>, coopmux:<account>, env:<VAR>", cred)
	}
	isCodex := strings.EqualFold(strings.TrimSpace(provider), "openai")
	if isCodex && scheme != "secret" {
		return parsedCredential{}, fmt.Errorf(
			"provider openai (Codex subscription login) needs --credential secret:<name>#%s, got scheme %q",
			codexAuthCredentialKey, scheme)
	}
	switch scheme {
	case "secret":
		name, key, hasKey := strings.Cut(target, "#")
		if name == "" {
			return parsedCredential{}, fmt.Errorf("malformed --credential %q: empty secret name", cred)
		}
		if isCodex {
			if hasKey && key != codexAuthCredentialKey {
				return parsedCredential{}, fmt.Errorf(
					"unsupported credential key %q in %q: provider openai reads the %q key of the secret",
					key, cred, codexAuthCredentialKey)
			}
			return parsedCredential{Scheme: scheme, Name: name, Key: codexAuthCredentialKey}, nil
		}
		if hasKey && key != "token" {
			return parsedCredential{}, fmt.Errorf(
				"unsupported credential key %q in %q: the controller reads the \"token\" key of a secret; drop the #%s suffix or store the value under the token key",
				key, cred, key)
		}
		return parsedCredential{Scheme: scheme, Name: name, Key: "token"}, nil
	case "coopmux", "env":
		return parsedCredential{Scheme: scheme, Name: target}, nil
	default:
		return parsedCredential{}, fmt.Errorf(
			"unsupported credential scheme %q in %q: use secret:<name>, coopmux:<account>, or env:<VAR>", scheme, cred)
	}
}

// isBedrockRouted reports whether an account routes through AWS Bedrock, based
// on its provider or model. Bedrock accounts carry provider "bedrock" (or a
// "bedrock-" variant) or a "bedrock-" model id.
func isBedrockRouted(provider, model string) bool {
	return strings.HasPrefix(strings.ToLower(provider), "bedrock") ||
		strings.HasPrefix(strings.ToLower(model), "bedrock-")
}

// looksLikeOAuthSecret reports whether a K8s Secret name appears to hold a
// sk-ant- shaped Anthropic OAuth token. The gardenio convention names these
// gardenio-claude-oauth-<user>; any secret whose name contains "oauth" is
// treated as one. This is the signal behind the Bedrock guard (ka-0000005094):
// such a token must never back a Bedrock-routed account.
func looksLikeOAuthSecret(name string) bool {
	return strings.Contains(strings.ToLower(name), "oauth")
}

func init() {
	accountCreateCmd.Flags().StringVar(&accountCreateProject, "project", "", "project to wire this account to (sets the project:<p> label) (required)")
	accountCreateCmd.Flags().StringVar(&accountCreateProvider, "provider", "", "provider, e.g. anthropic, openai, bedrock (required)")
	accountCreateCmd.Flags().StringVar(&accountCreateCredential, "credential", "", "credential reference: secret:<name>[#key], coopmux:<account>, or env:<VAR> (required)")
	accountCreateCmd.Flags().StringVar(&accountCreateModel, "model", "", "default model for agents using this account")
	accountCreateCmd.Flags().StringVar(&accountCreateBaseURL, "base-url", "", "API endpoint override (e.g. a LiteLLM proxy URL)")
	accountCreateCmd.Flags().StringVar(&accountCreateLiteLLMKey, "litellm-key", "", "LiteLLM virtual key for budget tracking")
	accountCreateCmd.Flags().StringVar(&accountCreateFallback, "fallback", "", "fallback account name when this one is exhausted")

	accountCmd.AddCommand(accountCreateCmd)
}
