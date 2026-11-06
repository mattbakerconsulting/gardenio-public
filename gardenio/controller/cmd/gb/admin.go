package main

import (
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"

	"gardenio/controller/internal/beadsapi"

	"github.com/spf13/cobra"
)

var adminCmd = &cobra.Command{
	Use:   "admin",
	Short: "Administrative operations (user management, session revocation)",
	Long: `Administrative operations requiring elevated privileges.

Subcommands:
  revoke-user   Revoke all Keycloak sessions for a user (offboarding)`,
	GroupID: "orchestration",
}

var adminRevokeUserCmd = &cobra.Command{
	Use:   "revoke-user",
	Short: "Revoke all Keycloak sessions for a user",
	Long: `Immediately revoke all active Keycloak sessions for a user.

Use this for employee offboarding or when a user's access must be terminated
immediately. The command looks up the user by email or username, counts their
active sessions, and revokes them all via the Keycloak Admin API.

With --disable, the user's Keycloak account is also disabled to prevent new
logins. This is recommended for permanent offboarding.

With --dry-run, the command previews what would happen without making changes.

Examples:
  gb admin revoke-user --email alice@example.com
  gb admin revoke-user --email alice@example.com --disable --reason "offboarded 2026-04-14"
  gb admin revoke-user --username alice --dry-run
  gb admin revoke-user --email alice@example.com --json`,
	RunE: func(cmd *cobra.Command, args []string) error {
		email, _ := cmd.Flags().GetString("email")
		username, _ := cmd.Flags().GetString("username")
		disable, _ := cmd.Flags().GetBool("disable")
		dryRun, _ := cmd.Flags().GetBool("dry-run")
		reason, _ := cmd.Flags().GetString("reason")

		if email == "" && username == "" {
			return fmt.Errorf("--email or --username is required")
		}

		req := beadsapi.RevokeUserRequest{
			Email:    email,
			Username: username,
			Disable:  disable,
			DryRun:   dryRun,
			Reason:   reason,
		}

		resp, err := daemon.RevokeUser(cmd.Context(), req)
		if err != nil {
			return err
		}

		if jsonOutput {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(resp)
		}

		if dryRun {
			fmt.Println("DRY RUN — no changes made")
			fmt.Println()
		}

		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintf(tw, "User ID:\t%s\n", resp.UserID)
		fmt.Fprintf(tw, "Username:\t%s\n", resp.Username)
		fmt.Fprintf(tw, "Email:\t%s\n", resp.Email)
		fmt.Fprintf(tw, "Sessions revoked:\t%d\n", resp.SessionsRevoked)
		if resp.UserDisabled {
			fmt.Fprintf(tw, "Account disabled:\tyes\n")
		}
		tw.Flush()

		if !dryRun && resp.SessionsRevoked > 0 {
			fmt.Printf("\nAll %d session(s) revoked successfully.\n", resp.SessionsRevoked)
		}
		if !dryRun && resp.SessionsRevoked == 0 {
			fmt.Println("\nNo active sessions found.")
		}

		return nil
	},
}

func init() {
	adminCmd.AddCommand(adminRevokeUserCmd)

	adminRevokeUserCmd.Flags().String("email", "", "user email address")
	adminRevokeUserCmd.Flags().String("username", "", "Keycloak username")
	adminRevokeUserCmd.Flags().Bool("disable", false, "also disable the user account (prevents new logins)")
	adminRevokeUserCmd.Flags().Bool("dry-run", false, "preview only, don't revoke")
	adminRevokeUserCmd.Flags().String("reason", "", "reason for revocation (recorded in audit log)")
}
