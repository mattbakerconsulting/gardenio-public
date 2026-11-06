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

var secretCmd = &cobra.Command{
	Use:     "secret",
	Short:   "Manage per-project pod secret env mappings",
	GroupID: "orchestration",
}

var (
	secretAddEnv    string
	secretAddSecret string
	secretAddKey    string
)

var secretAddCmd = &cobra.Command{
	Use:   "add <project> --env <ENVNAME> --secret <secret-name> --key <json-key>",
	Short: "Append (or update) a secret env mapping on a project bead",
	Long: `Adds an entry to the per-project secrets[] array on a type:project bead.

Each entry maps a Kubernetes Secret key to a pod environment variable: at pod
creation the controller injects <ENVNAME> from the Secret <secret-name>'s
<json-key> property. The write is a read-modify-write of the project bead's
"secrets" field and is idempotent — re-adding the same --env updates the
existing entry in place instead of duplicating it.

gb does NOT create the underlying value. After writing the mapping, this
command prints the exact AWS Secrets Manager command the operator still owes,
so the Secret (synced into the cluster from gardenio/<secret-name>) actually
carries the value the pod will read.

The controller mounts the Secret only if its name starts with "<project>-",
is not a controller-only credential, and matches the deployment's
agents.secretAllowlist (AGENT_SECRET_ALLOWLIST) when one is set. A name off
that list is dropped with a controller warning; add it to the list first.`,
	Example: `  gb secret add gardenio --env GITLAB_TOKEN --secret gardenio-gitlab-token --key token
  gb secret add daily-bundle --env GIT_USERNAME --secret daily-bundle-git-credentials --key username`,
	Args: cobra.ExactArgs(1),
	RunE: runSecretAdd,
}

func runSecretAdd(cmd *cobra.Command, args []string) error {
	project := args[0]
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	if secretAddEnv == "" {
		return fmt.Errorf("--env is required (the pod environment variable name)")
	}
	if secretAddSecret == "" {
		return fmt.Errorf("--secret is required (the Kubernetes Secret name)")
	}
	if secretAddKey == "" {
		return fmt.Errorf("--key is required (the property within the Secret)")
	}

	// Resolve the project bead to get its ID. A clear error here is the most
	// common failure mode (typo'd project name / project bead never created).
	info, err := daemon.GetProjectBead(ctx, project)
	if err != nil {
		return fmt.Errorf("resolving project %q: %w (is there a type:project bead with this title?)", project, err)
	}
	if info.BeadID == "" {
		return fmt.Errorf("project %q has no config bead ID; cannot write secrets", project)
	}

	// Re-fetch the raw bead for the read-modify-write. GetProjectBead above
	// resolved the ID via ListProjectBeads, whose unmarshalProjectField
	// SILENTLY DROPS a malformed secrets[] blob — so its parsed view can't be
	// trusted as the basis for a write. Reading the raw field here lets us
	// refuse on garbage rather than clobber an operator's existing entries.
	bead, err := daemon.GetBead(ctx, info.BeadID)
	if err != nil {
		return fmt.Errorf("reading project bead %s: %w", info.BeadID, err)
	}

	entries, err := parseSecretEntries(bead.Fields["secrets"])
	if err != nil {
		return fmt.Errorf("project %q (bead %s) has a malformed secrets[] field: %w; fix it by hand before using gb secret add", project, info.BeadID, err)
	}

	newEntry := beadsapi.SecretEntry{Env: secretAddEnv, Secret: secretAddSecret, Key: secretAddKey}
	entries, updated := upsertSecretEntry(entries, newEntry)

	if err := daemon.UpdateBeadFieldsTyped(ctx, info.BeadID, map[string]any{
		"secrets": entries,
	}); err != nil {
		return fmt.Errorf("writing secrets[] to project bead %s: %w", info.BeadID, err)
	}

	verb := "Added"
	if updated {
		verb = "Updated"
	}
	fmt.Fprintf(os.Stdout, "%s secret mapping on project %q (bead %s):\n", verb, project, info.BeadID)
	fmt.Fprintf(os.Stdout, "  env %s -> secret %q key %q\n", newEntry.Env, newEntry.Secret, newEntry.Key)

	fmt.Fprintln(os.Stdout, "\ngb did NOT create the AWS secret. You still owe the value in AWS Secrets Manager:")
	fmt.Fprintf(os.Stdout, "\n  %s\n", awsCreateSecretHint(newEntry.Secret, newEntry.Key))
	fmt.Fprintln(os.Stdout, "\nIf gardenio/"+newEntry.Secret+" already exists, set just this key with:")
	fmt.Fprintf(os.Stdout, "\n  %s\n", awsPutSecretValueHint(newEntry.Secret, newEntry.Key))

	return nil
}

// parseSecretEntries decodes the raw "secrets" field of a project bead into a
// slice of SecretEntry. The field is stored inconsistently across beads — as a
// native JSON array, or (legacy) as a JSON string whose content is the array
// text. fieldsMap() normalizes both to the array text, so a single Unmarshal
// handles them. An empty / null field is an empty slice (not an error); any
// other non-array shape is a hard error so we never silently discard the
// operator's existing entries.
func parseSecretEntries(raw string) ([]beadsapi.SecretEntry, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "null" {
		return nil, nil
	}
	var entries []beadsapi.SecretEntry
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		return nil, fmt.Errorf("expected a JSON array of {env,secret,key}: %w", err)
	}
	return entries, nil
}

// upsertSecretEntry appends want, or — if an entry with the same Env already
// exists — replaces it in place. Returns the resulting slice and whether an
// existing entry was updated (vs appended). Matching on Env makes the command
// idempotent: re-running with the same --env never duplicates a mapping.
func upsertSecretEntry(entries []beadsapi.SecretEntry, want beadsapi.SecretEntry) ([]beadsapi.SecretEntry, bool) {
	for i := range entries {
		if entries[i].Env == want.Env {
			entries[i] = want
			return entries, true
		}
	}
	return append(entries, want), false
}

// awsCreateSecretHint returns the exact, copy-pasteable AWS CLI command the
// operator owes to create the Secrets Manager entry behind a project secret.
// The value is left as the literal placeholder <value> for the operator to
// fill. The path is gardenio/<secret-name> with a single JSON property matching
// the mapping's key.
func awsCreateSecretHint(secretName, key string) string {
	return fmt.Sprintf(
		`aws secretsmanager create-secret --name gardenio/%s --secret-string '{"%s":"<value>"}'`,
		secretName, key)
}

// awsPutSecretValueHint returns the put-secret-value form for when the
// gardenio/<secret-name> entry already exists and only this key is being
// added/replaced.
func awsPutSecretValueHint(secretName, key string) string {
	return fmt.Sprintf(
		`aws secretsmanager put-secret-value --secret-id gardenio/%s --secret-string '{"%s":"<value>"}'`,
		secretName, key)
}

func init() {
	secretAddCmd.Flags().StringVar(&secretAddEnv, "env", "", "pod environment variable name to inject (required)")
	secretAddCmd.Flags().StringVar(&secretAddSecret, "secret", "", "Kubernetes Secret name holding the value (required)")
	secretAddCmd.Flags().StringVar(&secretAddKey, "key", "token", "property within the Secret to read")

	secretCmd.AddCommand(secretAddCmd)
	rootCmd.AddCommand(secretCmd)
}
