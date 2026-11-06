package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"gardenio/controller/internal/lands/provision"

	"github.com/spf13/cobra"
)

// gb lands provision and gb lands close are the operator's two handles on the
// Lands provisioner (Track B spec §3.3, step 8.6;
// internal/lands/provision). The provisioner runs in the controller, which
// holds the Keycloak and LiteLLM credentials; these commands only read the
// plot application and write the request onto it, and the controller's next
// pass (within a minute) does the work. Both are admin only:
// plot_application is admin-gated in kbeads.

var landsProvisionCmd = &cobra.Command{
	Use:   "provision <application|land>",
	Short: "Show a plot application's provisioning, or --retry a failed one (admin only)",
	Long: `Shows where the Lands provisioner is with a plot application: its state, the
last step done (provision_step), the attempt and the error it stopped on.

With --retry, a failed application goes back to provisioning as a new
attempt; the controller resumes it after the last step done, and files a new
escalation if it fails again. The argument is the plot_application id or the
land name.`,
	Args: cobra.ExactArgs(1),
	RunE: runLandsProvision,
}

var landsCloseCmd = &cobra.Command{
	Use:   "close <land|application>",
	Short: "Close a tenant land: freeze, stop agents, remove members, revoke the key, close the project (admin only)",
	Long: `Asks the Lands provisioner to close a land. On its next pass the controller
runs provisioning in reverse: it freezes the land, stops its agents, takes its
members out of the land's groups (and out of lands-tenants when it was their
last land), revokes the land's LiteLLM key, closes the project bead with
land_closed_at, mails the tenant and marks the application closed. The
land's beads are kept.

A close that fails stops and files an escalation; run gb lands close again
once the cause is fixed, and it resumes from the step that failed.`,
	Args: cobra.ExactArgs(1),
	RunE: runLandsClose,
}

// provisionView is gb lands provision's --json output.
type provisionView struct {
	Application string `json:"application"`
	Land        string `json:"land"`
	State       string `json:"state"`
	Step        string `json:"provision_step"`
	Attempt     int    `json:"provision_attempt"`
	Error       string `json:"provision_error,omitempty"`
	CloseStep   string `json:"close_step,omitempty"`
	CloseAsked  string `json:"close_requested_at,omitempty"`
}

func provisionViewOf(app *provision.Application) provisionView {
	return provisionView{
		Application: app.ID, Land: app.LandName, State: app.State, Step: app.Step, Attempt: app.Attempt,
		Error: app.Error, CloseStep: app.CloseStep, CloseAsked: app.CloseRequestedAt,
	}
}

func printProvision(w io.Writer, app *provision.Application) {
	v := provisionViewOf(app)
	fmt.Fprintf(w, "%s  land %s  %s\n", v.Application, v.Land, v.State)
	fmt.Fprintf(w, "  provision_step: %s  attempt: %d\n", orNone(v.Step), v.Attempt)
	if v.CloseAsked != "" {
		fmt.Fprintf(w, "  close requested %s  close_step: %s\n", v.CloseAsked, orNone(v.CloseStep))
	}
	if v.Error != "" {
		fmt.Fprintf(w, "  error: %s\n", v.Error)
	}
}

// retryProvision finds ref and retries it.
func retryProvision(ctx context.Context, d provision.Daemon, ref string) (*provision.Application, error) {
	app, err := provision.Find(ctx, d, ref)
	if err != nil {
		return nil, err
	}
	if err := provision.Retry(ctx, d, app); err != nil {
		return nil, err
	}
	return app, nil
}

// requestLandClose finds ref and asks for its close.
func requestLandClose(ctx context.Context, d provision.Daemon, ref, by, reason string, now time.Time) (*provision.Application, error) {
	app, err := provision.Find(ctx, d, ref)
	if err != nil {
		return nil, err
	}
	if err := provision.RequestClose(ctx, d, app, by, reason, now); err != nil {
		return nil, err
	}
	return app, nil
}

func runLandsProvision(cmd *cobra.Command, args []string) error {
	retry, _ := cmd.Flags().GetBool("retry")
	var (
		app *provision.Application
		err error
	)
	if retry {
		app, err = retryProvision(cmd.Context(), daemon, args[0])
	} else {
		app, err = provision.Find(cmd.Context(), daemon, args[0])
	}
	if err != nil {
		return err
	}
	if jsonOutput {
		return json.NewEncoder(os.Stdout).Encode(provisionViewOf(app))
	}
	if retry {
		fmt.Printf("retrying %s (land %s) as attempt %d; the controller resumes after step %s within a minute\n",
			app.ID, app.LandName, app.Attempt, orNone(app.Step))
		return nil
	}
	printProvision(os.Stdout, app)
	return nil
}

func runLandsClose(cmd *cobra.Command, args []string) error {
	reason, _ := cmd.Flags().GetString("reason")
	app, err := requestLandClose(cmd.Context(), daemon, args[0], landsOperator(), reason, time.Now())
	if err != nil {
		return err
	}
	if jsonOutput {
		return json.NewEncoder(os.Stdout).Encode(provisionViewOf(app))
	}
	fmt.Printf("close of land %s (%s) requested; the controller closes it within a minute (gb lands provision %s shows progress)\n",
		app.LandName, app.ID, app.ID)
	return nil
}

func init() {
	landsProvisionCmd.Flags().Bool("retry", false, "move a failed application back to provisioning as a new attempt")
	landsCloseCmd.Flags().String("reason", "", "why the land is closed; the tenant's mail quotes it")
	landsCmd.AddCommand(landsProvisionCmd, landsCloseCmd)
}
