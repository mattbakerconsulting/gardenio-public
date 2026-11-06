package main

import (
	"fmt"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"github.com/spf13/cobra"
)

// consent_push_manage.go implements commands to manage standing consent for
// automatic MR pushes. The consent is the gated type:user field
// consent_push_to_mr_auto (gd-QV2WuUjely): a person gives or withdraws their own
// with opt-in / opt-out (their own OIDC login), and grant / revoke for someone
// else need an OIDC gardenio-admin. kbeads refuses both from an agent's token.

var consentPushManageCmd = &cobra.Command{
	Use:     "consent-to-push-manage",
	Short:   "Manage user opt-in consent for automatic MR pushes",
	GroupID: "orchestration",
	Long: `Manage user opt-in consent for automatic MR pushes.

Users can opt in to have the pipeline failure analyzer push fixes directly to their MRs
without requiring a manual consent decision prompt first. The beads daemon answers a
consent-to-push decision addressed to someone who opted in (responded_by
system:standing-consent); no agent can opt anyone in.

Subcommands:
  opt-in           Give your own standing consent (your own OIDC login)
  opt-out          Withdraw your own standing consent
  status           Show whether you have given standing consent
  grant <user>     Grant consent for a user (OIDC gardenio-admin; by email or Slack id)
  revoke <user>    Revoke consent for a user (OIDC gardenio-admin)
  check <user>     Check if a user has consented
  list             List all users who have consented`,
}

var consentPushOptInCmd = &cobra.Command{
	Use:   "opt-in",
	Short: "Give your own standing consent to automatic MR pushes",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return setMyConsent(cmd, true)
	},
}

var consentPushOptOutCmd = &cobra.Command{
	Use:   "opt-out",
	Short: "Withdraw your own standing consent to automatic MR pushes",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return setMyConsent(cmd, false)
	},
}

var consentPushStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show whether you have given standing consent to automatic MR pushes",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := daemon.MyConsentPush(cmd.Context())
		if err != nil {
			return fmt.Errorf("reading your standing consent (needs your own OIDC login): %w", err)
		}
		printMyConsent(c)
		return nil
	},
}

func setMyConsent(cmd *cobra.Command, auto bool) error {
	c, err := daemon.SetMyConsentPush(cmd.Context(), auto)
	if err != nil {
		return fmt.Errorf("setting your standing consent (needs your own OIDC login, not an agent token): %w", err)
	}
	printMyConsent(c)
	return nil
}

func printMyConsent(c *beadsapi.ConsentPush) {
	if c.Auto {
		fmt.Printf("✓ Standing consent given: CI fixes addressed to you are pushed to your MRs without a prompt (user bead %s)\n", c.UserBeadID)
	} else {
		fmt.Printf("✗ No standing consent: you will be asked for each fix (user bead %s)\n", c.UserBeadID)
	}
}

var consentPushGrantCmd = &cobra.Command{
	Use:   "grant <user>",
	Short: "Grant consent for a user",
	Long: `Grant opt-in consent for a user to have CI fixes pushed automatically to their MRs.

Needs an OIDC gardenio-admin login; the person can instead run 'opt-in' themself.
User can be specified as an email or Slack ID:
  gb consent-to-push-manage grant user@example.com
  gb consent-to-push-manage grant U0AB1CD2EF
  gb consent-to-push-manage grant <@U0AB1CD2EF>`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		user := args[0]
		return grantConsentForUser(cmd, user)
	},
}

var consentPushRevokeCmd = &cobra.Command{
	Use:   "revoke <user>",
	Short: "Revoke consent for a user",
	Long:  `Revoke opt-in consent for a user (they will be prompted again for each fix).`,
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		user := args[0]
		return revokeConsentForUser(cmd, user)
	},
}

var consentPushCheckCmd = &cobra.Command{
	Use:   "check <user>",
	Short: "Check if a user has consented",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		user := args[0]
		return checkConsentForUser(cmd, user)
	},
}

var consentPushListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all users who have consented",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return listConsentedUsers(cmd)
	},
}

func grantConsentForUser(cmd *cobra.Command, user string) error {
	return setConsentForUser(cmd, user, true)
}

func revokeConsentForUser(cmd *cobra.Command, user string) error {
	return setConsentForUser(cmd, user, false)
}

// setConsentForUser writes the gated consent field on another person's user
// bead. kbeads refuses it (403) unless the caller is an OIDC gardenio-admin.
func setConsentForUser(cmd *cobra.Command, user string, auto bool) error {
	bead, err := resolveConsentUser(cmd, user)
	if err != nil {
		return err
	}
	if err := daemon.UpdateBeadFieldsTyped(cmd.Context(), bead.ID, map[string]any{
		consentPushAutoField: auto,
	}); err != nil {
		return fmt.Errorf("updating consent field (needs an OIDC gardenio-admin login; the person can run 'gb consent-to-push-manage opt-in' themself): %w", err)
	}

	verb := "Granted"
	if !auto {
		verb = "Revoked"
	}
	fmt.Printf("%s consent for user %s (%s): %s\n", verb, bead.Title, bead.Fields["email"], bead.ID)
	return nil
}

func checkConsentForUser(cmd *cobra.Command, user string) error {
	bead, err := resolveConsentUser(cmd, user)
	if err != nil {
		return err
	}

	fmt.Printf("User: %s (%s)\n", bead.Title, bead.ID)
	fmt.Printf("Email: %s\n", bead.Fields["email"])
	fmt.Printf("Slack ID: %s\n", bead.Fields["slack_id"])
	if hasConsent(bead) {
		fmt.Printf("Status: ✓ Has opted in to automatic MR pushes\n")
	} else {
		fmt.Printf("Status: ✗ Has NOT opted in (will receive consent prompts)\n")
	}

	return nil
}

func listConsentedUsers(cmd *cobra.Command) error {
	beads, err := daemon.ListBeadsFiltered(cmd.Context(), beadsapi.ListBeadsQuery{
		Types:    []string{"user"},
		Kinds:    []string{"config"},
		Statuses: []string{"open", "in_progress"},
		Limit:    10000,
	})
	if err != nil {
		return fmt.Errorf("listing user beads: %w", err)
	}

	var consentedUsers []*beadsapi.BeadDetail
	for _, bead := range beads.Beads {
		if hasConsent(bead) {
			consentedUsers = append(consentedUsers, bead)
		}
	}

	if len(consentedUsers) == 0 {
		fmt.Println("No users have opted in to automatic MR pushes")
		return nil
	}

	fmt.Printf("Users with automatic MR push consent (%d):\n\n", len(consentedUsers))
	for _, bead := range consentedUsers {
		fmt.Printf("- %s (%s) [%s]\n", bead.Fields["display_name"], bead.Fields["email"], bead.ID)
		if slack := bead.Fields["slack_id"]; slack != "" {
			fmt.Printf("  Slack: %s\n", slack)
		}
	}

	return nil
}

// resolveConsentUser resolves a user bead from an email, Slack id, or mention,
// reusing the shared findUserBead resolver after normalizing the input into the
// operated_by vocabulary.
func resolveConsentUser(cmd *cobra.Command, user string) (*beadsapi.BeadDetail, error) {
	normalized, err := normalizeOperatedBy(user)
	if err != nil {
		return nil, err
	}
	// normalizeOperatedBy returns "<kind>:<identifier>"; findUserBead resolves by
	// the bare identifier (email / Slack id / username).
	identifier := normalized
	if _, rest, ok := strings.Cut(normalized, ":"); ok {
		identifier = rest
	}
	return findUserBead(cmd, identifier)
}

func init() {
	consentPushManageCmd.AddCommand(consentPushOptInCmd)
	consentPushManageCmd.AddCommand(consentPushOptOutCmd)
	consentPushManageCmd.AddCommand(consentPushStatusCmd)
	consentPushManageCmd.AddCommand(consentPushGrantCmd)
	consentPushManageCmd.AddCommand(consentPushRevokeCmd)
	consentPushManageCmd.AddCommand(consentPushCheckCmd)
	consentPushManageCmd.AddCommand(consentPushListCmd)
	rootCmd.AddCommand(consentPushManageCmd)
}
