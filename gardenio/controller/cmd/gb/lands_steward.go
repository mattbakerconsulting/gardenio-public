package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"gardenio/controller/internal/steward"

	"github.com/spf13/cobra"
)

// gb lands steward shows a land's steward, or binds one (step 9.13). The
// Lands provisioner binds a tenant land's steward when the land opens; the
// home garden (steward.HomeOwner) is no provisioned land, so its steward is
// bound here, once, by the operator.

var landsStewardCmd = &cobra.Command{
	Use:   "steward <land>",
	Short: "Show a land's steward, or --bind one (the home garden's is bound here)",
	Long: `With no flags, prints the land's steward trigger, or says it has none.

--bind binds the land's one steward: a standing gardener trigger that spawns
the steward role on the land's wakes and its daily digest, and the
steward-notes:<land> bead when the land has none. A land that has a steward
keeps it, so a second --bind changes nothing but its wakes, which it brings up
to date with a fresh bind's (the person-joined wake, step 9.14, and on the home
garden the quiet-gardeners watch, gd-bo2RUmtpHb). The Lands provisioner binds a
tenant land's steward when the land opens; --bind is how the home garden (the
operator land on the Commons whose operator is a person) gets one. The home
garden's steward wakes on an @steward mention, its decisions answered, its
notes edited, the digest, and every two hours when a plan's gardener is quiet
or still timed (a preflight trigger-engine checks without spawning); its purse
is unmetered.`,
	Example: `  gb lands steward gardenio
  gb lands steward gardenio --bind`,
	Args: cobra.ExactArgs(1),
	RunE: runLandsSteward,
}

func runLandsSteward(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	land := strings.TrimSpace(args[0])
	if bind, _ := cmd.Flags().GetBool("bind"); bind {
		upgraded := false
		res, err := steward.Bind(ctx, daemon, land, steward.BindSpec{CreatedBy: strings.TrimSpace(actor)})
		if err != nil {
			return err
		}
		if res.Adopted {
			// A steward bound before a step added a wake (person-joined,
			// step 9.14) gets it now; the provisioner's pass does this for
			// a tenant land, and nothing passes over the home garden.
			b, err := daemon.GetBead(ctx, res.ID)
			if err != nil {
				return fmt.Errorf("reading %s's steward %s: %w", land, res.ID, err)
			}
			home, err := steward.IsHome(ctx, daemon, land)
			if err != nil {
				return err
			}
			hook := steward.ProvisionHook{Client: daemon}
			if home {
				// The home steward also gains the quiet-gardeners watch
				// (gd-bo2RUmtpHb).
				upgraded, err = hook.UpgradeHome(ctx, land, b)
			} else {
				err = hook.Upgrade(ctx, land, b)
			}
			if err != nil {
				return err
			}
		}
		if jsonOutput {
			return json.NewEncoder(os.Stdout).Encode(res)
		}
		if res.Adopted {
			state := "its wakes are up to date"
			if upgraded {
				state = "its wakes are brought up to date"
			}
			fmt.Printf("%s already has a steward: %s (notes %s); %s\n", land, res.ID, res.NotesID, state)
			return nil
		}
		fmt.Printf("bound %s's steward %s with %d wakes (notes %s)\n", land, res.ID, len(res.Triggers), res.NotesID)
		fmt.Printf("say @%s in a channel of %s to wake it\n", steward.Role, land)
		return nil
	}
	b, err := steward.FindSteward(ctx, daemon, land)
	if err != nil {
		return err
	}
	if b == nil {
		return fmt.Errorf("land %s has no steward; bind one with gb lands steward %s --bind", land, land)
	}
	if jsonOutput {
		return json.NewEncoder(os.Stdout).Encode(map[string]string{"id": b.ID, "status": b.Status, "enabled": b.Fields["enabled"]})
	}
	fmt.Printf("%s's steward: %s (%s, enabled %s)\n", land, b.ID, b.Status, orNone(b.Fields["enabled"]))
	return nil
}

func init() {
	landsStewardCmd.Flags().Bool("bind", false, "bind the land's steward if it has none")
	landsCmd.AddCommand(landsStewardCmd)
}
