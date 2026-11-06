package main

import (
	"fmt"

	"gardenio/clicreds/credstore"
	"gardenio/clicreds/oidclogin"

	"github.com/spf13/cobra"
)

var logoutContext string

var logoutCmd = &cobra.Command{
	Use:     "logout",
	Short:   "Forget the Keycloak session and stored credentials",
	GroupID: "session",
	Long: `Forget the Keycloak session 'gb login' / 'kd login' cached, and remove the
current (or named) credential context an older gb login stored.

Examples:
  gb logout                # sign out and clear the current context
  gb logout --context prod # clear only a specific credential context`,
	PersistentPreRunE: func(cmd *cobra.Command, _ []string) error { cmd.SilenceUsage = true; return nil },
	RunE: func(_ *cobra.Command, _ []string) error {
		if logoutContext == "" {
			removed, err := oidclogin.Remove()
			if err != nil {
				return fmt.Errorf("removing login session: %w", err)
			}
			if removed {
				fmt.Println("Signed out of the Keycloak session (shared with kd).")
			}
		}
		store, err := credstore.Load("")
		if err != nil {
			return fmt.Errorf("loading credential store: %w", err)
		}
		ctxName := logoutContext
		if ctxName == "" {
			ctxName = store.CurrentContext()
		}
		if store.Context(ctxName) == nil {
			fmt.Printf("No credentials stored for context %q.\n", ctxName)
			return nil
		}
		store.DeleteContext(ctxName)
		if err := store.Save(); err != nil {
			return fmt.Errorf("saving credential store: %w", err)
		}
		fmt.Printf("Credentials cleared for context %q.\n", ctxName)
		return nil
	},
}

func init() {
	logoutCmd.Flags().StringVar(&logoutContext, "context", "", "credential context to clear (default: current)")
}
