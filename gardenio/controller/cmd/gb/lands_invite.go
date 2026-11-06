package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/mail"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/lands/provision"

	"github.com/spf13/cobra"
)

// gb lands invite|invites|uninvite: sign-in invites by email (owner ruling
// gd-hCMlZ5jnvB = b, gd-ycE484kLyk). GitHub sign-in is invite-only; the
// gardenio.clonk.tv edge admits /lands/ only to /invited and the groups it
// already admitted. gb files a type=invite bead (admin-gated, kbeads
// invite.go); the controller's Lands provisioner mails the address and, once
// someone signs in with GitHub and confirms that address, adds them to
// /invited (and to the land, when the invite names one).

var landsInviteCmd = &cobra.Command{
	Use:   "invite <email>",
	Short: "Invite someone, by email, to sign in to Lands with GitHub",
	Long: `Files a sign-in invite (type=invite, admin only) for one email address. The
controller mails the address a link to the Lands front door. When someone
signs in with GitHub and confirms exactly that address, they join the Keycloak
group invited, which is what gets a GitHub sign-in past gardenio.clonk.tv's
/lands/ check, and the invite is claimed. With --land they also join that
tenant land with --role (member by default).

The invite is open for 14 days (--ttl). The address is matched exactly,
lowercased; Keycloak verifies it, so a GitHub account with someone else's
address never matches. gb lands uninvite ends a pending one.`,
	Example: `  gb lands invite someone@example.com
  gb lands invite someone@example.com --land tomato-patch --role guest`,
	Args: cobra.ExactArgs(1),
	RunE: runLandsInvite,
}

var landsInvitesCmd = &cobra.Command{
	Use:   "invites",
	Short: "List the pending sign-in invites (--all for the ended ones too)",
	Args:  cobra.NoArgs,
	RunE:  runLandsInvites,
}

var landsUninviteCmd = &cobra.Command{
	Use:   "uninvite <invite-id>",
	Short: "End a pending sign-in invite before it is claimed",
	Long: `Asks the controller to revoke a pending sign-in invite: it is closed on the
next provisioner pass and never lets anyone in. A claimed invite is already
closed; to take a person's access away, remove them from the Keycloak group
invited (and from any land).`,
	Args: cobra.ExactArgs(1),
	RunE: runLandsUninvite,
}

// inviteStore is the slice of the daemon client the invite commands use.
type inviteStore interface {
	CreateBeadDetail(ctx context.Context, req beadsapi.CreateBeadRequest) (*beadsapi.BeadDetail, error)
	ListAllBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
	GetBead(ctx context.Context, beadID string) (*beadsapi.BeadDetail, error)
	UpdateBeadFieldsTyped(ctx context.Context, beadID string, fields map[string]any) error
}

// inviteRequest is one gb lands invite.
type inviteRequest struct {
	Email     string
	Land      string
	Role      string
	InvitedBy string
	TTL       time.Duration
}

// normalizeInviteEmail lowercases a bare address and refuses anything else
// (a display name, two addresses, no @).
func normalizeInviteEmail(raw string) (string, error) {
	email := strings.ToLower(strings.TrimSpace(raw))
	a, err := mail.ParseAddress(email)
	if err != nil || a.Name != "" || a.Address != email || strings.Count(email, "@") != 1 {
		return "", fmt.Errorf("%q is not one bare email address", raw)
	}
	return email, nil
}

// fileInvite writes the invite bead.
func fileInvite(ctx context.Context, st inviteStore, in inviteRequest, now time.Time) (*beadsapi.BeadDetail, error) {
	email, err := normalizeInviteEmail(in.Email)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.InvitedBy) == "" {
		return nil, fmt.Errorf("no operator identity to record as invited_by; run gb login")
	}
	if in.TTL < time.Hour {
		return nil, fmt.Errorf("--ttl %s: an invite is open for at least an hour", in.TTL)
	}
	fields := map[string]any{
		"email": email, "invited_by": in.InvitedBy, "state": provision.InvitePending,
		"expires_at": now.Add(in.TTL).UTC().Format(time.RFC3339),
	}
	switch {
	case in.Land != "":
		role := in.Role
		if role == "" {
			role = "member"
		}
		if role != "member" && role != "guest" {
			return nil, fmt.Errorf("--role %q: an invite to a land gives member or guest", role)
		}
		fields["land"], fields["role"] = in.Land, role
	case in.Role != "":
		return nil, fmt.Errorf("--role needs --land")
	}
	raw, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	created, err := st.CreateBeadDetail(ctx, beadsapi.CreateBeadRequest{
		Title:      "Sign-in invite for " + email,
		ShortTitle: "invite someone to sign in",
		Type:       provision.SigninInviteType,
		Kind:       "data",
		Fields:     raw,
	})
	if err != nil {
		return nil, fmt.Errorf("writing the invite: %w", err)
	}
	return created, nil
}

func runLandsInvite(cmd *cobra.Command, args []string) error {
	land, _ := cmd.Flags().GetString("land")
	role, _ := cmd.Flags().GetString("role")
	ttl, _ := cmd.Flags().GetDuration("ttl")
	b, err := fileInvite(cmd.Context(), daemon, inviteRequest{
		Email: args[0], Land: strings.TrimSpace(land), Role: strings.TrimSpace(role), InvitedBy: landsOperator(), TTL: ttl,
	}, time.Now())
	if err != nil {
		return err
	}
	if jsonOutput {
		return json.NewEncoder(os.Stdout).Encode(b)
	}
	to := ""
	if b.Fields["land"] != "" {
		to = fmt.Sprintf(" (and to %s as %s)", b.Fields["land"], b.Fields["role"])
	}
	fmt.Printf("invited %s to sign in%s: %s, open until %s\n", b.Fields["email"], to, b.ID, b.Fields["expires_at"])
	fmt.Println("the controller mails the address; it is claimed once they sign in with GitHub and confirm it")
	return nil
}

// listInvites returns the invites, open ones only unless all.
func listInvites(ctx context.Context, st inviteStore, all bool) ([]*beadsapi.BeadDetail, error) {
	q := beadsapi.ListBeadsQuery{Types: []string{provision.SigninInviteType}, Statuses: []string{"open", "in_progress", "blocked", "deferred"}}
	if all {
		q.Statuses = append(q.Statuses, "closed")
	}
	res, err := st.ListAllBeadsFiltered(ctx, q)
	if err != nil {
		return nil, err
	}
	return res.Beads, nil
}

func printInvites(w io.Writer, invites []*beadsapi.BeadDetail) {
	if len(invites) == 0 {
		fmt.Fprintln(w, "no invites")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tEMAIL\tSTATE\tLAND\tMAILED\tEXPIRES")
	for _, b := range invites {
		f := b.Fields
		land := orNone(f["land"])
		if f["land"] != "" {
			land += " (" + f["role"] + ")"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", b.ID, f["email"], f["state"], land, orNone(f["mailed_at"]), f["expires_at"])
	}
	_ = tw.Flush()
}

func runLandsInvites(cmd *cobra.Command, _ []string) error {
	all, _ := cmd.Flags().GetBool("all")
	invites, err := listInvites(cmd.Context(), daemon, all)
	if err != nil {
		return err
	}
	if jsonOutput {
		return json.NewEncoder(os.Stdout).Encode(invites)
	}
	printInvites(os.Stdout, invites)
	return nil
}

// requestUninvite asks the provisioner to revoke a pending invite.
func requestUninvite(ctx context.Context, st inviteStore, id, by string, now time.Time) error {
	b, err := st.GetBead(ctx, id)
	if err != nil {
		return err
	}
	if b.Type != provision.SigninInviteType {
		return fmt.Errorf("%s is a %s, not a sign-in invite", id, b.Type)
	}
	if s := b.Fields["state"]; s != provision.InvitePending {
		return fmt.Errorf("%s is %s, not pending; a claimed invite's access is taken away in Keycloak", id, s)
	}
	return st.UpdateBeadFieldsTyped(ctx, id, map[string]any{
		"revoke_requested_at": now.UTC().Format(time.RFC3339), "revoke_requested_by": by,
	})
}

func runLandsUninvite(cmd *cobra.Command, args []string) error {
	if err := requestUninvite(cmd.Context(), daemon, args[0], landsOperator(), time.Now()); err != nil {
		return err
	}
	fmt.Printf("%s: revoke requested; the controller closes it on its next pass\n", args[0])
	return nil
}

func init() {
	landsInviteCmd.Flags().String("land", "", "also join this tenant land when the invite is claimed")
	landsInviteCmd.Flags().String("role", "", "the role in --land: member (default) or guest")
	landsInviteCmd.Flags().Duration("ttl", provision.InviteTTL, "how long the invite stays open")
	landsInvitesCmd.Flags().Bool("all", false, "include claimed, revoked and expired invites")
	landsCmd.AddCommand(landsInviteCmd, landsInvitesCmd, landsUninviteCmd)
}
