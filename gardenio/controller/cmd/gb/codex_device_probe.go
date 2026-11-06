package main

// codex_device_probe.go — `gb codex device-probe`: run the controller's device
// login client (internal/codexauth.DeviceLogin) once against the real
// auth.openai.com and DISCARD the result. It exists to close ADR 0007 risks R-A/R-B
// (the wire protocol and OpenAI's acceptance of a non-CLI originator) before the
// controller broker ever touches gardenio-codex-auth. Nothing is stored; only
// non-secret facts about the result are printed.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"gardenio/controller/internal/codexauth"

	"github.com/spf13/cobra"
)

var codexDeviceProbeCmd = &cobra.Command{
	Use:   "device-probe",
	Short: "Exercise the device-login client against auth.openai.com and discard the result",
	Long: `Requests a device code with the same client the controller uses, prints the URL and
code on this terminal, waits for you to approve it in a browser, and completes the code
exchange. The tokens are discarded: only the plan account id, whether a refresh token
and id token came back, the access-token expiry, and a refresh-token fingerprint are
printed. Use it once to confirm the protocol before enabling the controller broker.

Approve with the account you would use for the cluster login. This signs that browser
session into a throwaway token family; it does not touch the cluster.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return codexDeviceProbe(cmd.Context(), codexauth.NewDeviceLogin(""), cmd.OutOrStdout(), cmd.ErrOrStderr())
	},
}

func init() { codexCmd.AddCommand(codexDeviceProbeCmd) }

func codexDeviceProbe(ctx context.Context, d *codexauth.DeviceLogin, stdout, stderr io.Writer) error {
	dc, err := d.Start(ctx)
	if err != nil {
		return fmt.Errorf("R-A/R-B: requesting a device code failed: %w", err)
	}
	fmt.Fprintf(stderr, "Open %s and enter the code %s (valid until %s). Waiting for approval...\n",
		dc.VerificationURL, dc.UserCode, dc.ExpiresAt.Local().Format(time.TimeOnly))
	tr, err := d.Poll(ctx, dc)
	if errors.Is(err, codexauth.ErrDeviceCodeExpired) {
		return errors.New("the code expired before it was approved; run again")
	}
	if err != nil {
		return fmt.Errorf("R-A/R-B: polling or exchange failed: %w", err)
	}
	a, err := codexauth.NewAuth(tr, time.Now())
	if err != nil {
		return fmt.Errorf("exchange returned an unusable login: %w", err)
	}
	exp, expOK := a.AccessExpiry()
	fmt.Fprintln(stdout, "device login protocol OK; result discarded")
	fmt.Fprintf(stdout, "  chatgpt_account_id present: %t\n", a.AccountID() != "")
	fmt.Fprintf(stdout, "  id_token present:           %t\n", a.IDToken() != "")
	fmt.Fprintf(stdout, "  refresh_token present:      %t (fingerprint %s)\n", a.RefreshToken() != "", a.RefreshFingerprint())
	if expOK {
		fmt.Fprintf(stdout, "  access token expires:       %s\n", exp.UTC().Format(time.RFC3339))
	} else {
		fmt.Fprintln(stdout, "  access token expiry:        unreadable")
	}
	return nil
}
