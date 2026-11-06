package main

// codex_login.go — `gb codex login`: the laptop fallback for a ChatGPT-plan login
// (ADR 0007 option D). It wraps the manual mint: a throwaway CODEX_HOME, `codex
// login --device-auth` (the operator approves in a browser), then the document goes
// into the account Secret with kubectl without ever being printed, and the
// throwaway home is removed. The preferred path is the controller-side device
// login (ADR 0007 option A); this exists for when that is unavailable and for
// bootstrapping a Secret that does not exist yet.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"gardenio/controller/internal/codexauth"

	"github.com/spf13/cobra"
)

var codexCmd = &cobra.Command{
	Use:     "codex",
	Short:   "Codex (ChatGPT-plan) login helpers",
	GroupID: "orchestration",
}

var codexLoginCmd = &cobra.Command{
	Use:   "login",
	Short: "Mint a ChatGPT-plan login into a gardenio-codex-auth Secret (laptop fallback)",
	Long: `Runs ` + "`codex login --device-auth`" + ` against a throwaway CODEX_HOME, waits for you to
approve the printed code in a browser signed in as the plan owner, and stores the
resulting auth.json in the Secret with kubectl, without displaying it. The throwaway
home is removed afterwards. Never copies ~/.codex/auth.json: a shared refresh-token
family would log the cluster and your laptop out of each other.

The controller's refresh keeper picks the new login up on its next pass and clears
probe_auth_status=rejected by itself. Prefer the "Start device login" button on the
re-login escalation when it is available; use this when it is not.`,
	Example: `  gb codex login --secret gardenio-codex-auth --namespace gardenio`,
	Args:    cobra.NoArgs,
	RunE:    runCodexLogin,
}

var codexLoginFlags struct {
	secret      string
	namespace   string
	kubeContext string
	codexBin    string
	kubectlBin  string
}

func init() {
	f := codexLoginCmd.Flags()
	f.StringVar(&codexLoginFlags.secret, "secret", "gardenio-codex-auth", "Secret to write (key auth.json)")
	f.StringVarP(&codexLoginFlags.namespace, "namespace", "n", "", "Kubernetes namespace of the Secret (default: kubectl's current)")
	f.StringVar(&codexLoginFlags.kubeContext, "context", "", "kubectl context (default: current)")
	f.StringVar(&codexLoginFlags.codexBin, "codex", "codex", "codex binary")
	f.StringVar(&codexLoginFlags.kubectlBin, "kubectl", "kubectl", "kubectl binary")
	codexCmd.AddCommand(codexLoginCmd)
	rootCmd.AddCommand(codexCmd)
}

// codexLoginDeps are the side effects, injectable for tests.
type codexLoginDeps struct {
	// runCodex runs `codex login --device-auth` with CODEX_HOME=home, attached to
	// the operator's terminal so they see the URL and code.
	runCodex func(ctx context.Context, bin, home string, stdout, stderr io.Writer) error
	// kubectl runs kubectl with args and stdin and returns its stdout. A failure
	// carries kubectl's stderr (never the document: it travels on stdin only).
	kubectl func(ctx context.Context, bin string, args []string, stdin []byte) ([]byte, error)
}

func defaultCodexLoginDeps() codexLoginDeps {
	return codexLoginDeps{
		runCodex: func(ctx context.Context, bin, home string, stdout, stderr io.Writer) error {
			c := exec.CommandContext(ctx, bin, "login", "--device-auth")
			c.Env = append(os.Environ(), "CODEX_HOME="+home)
			c.Stdin, c.Stdout, c.Stderr = os.Stdin, stdout, stderr
			return c.Run()
		},
		kubectl: func(ctx context.Context, bin string, args []string, stdin []byte) ([]byte, error) {
			var stdout, stderr bytes.Buffer
			c := exec.CommandContext(ctx, bin, args...)
			c.Stdin = bytes.NewReader(stdin)
			c.Stdout, c.Stderr = &stdout, &stderr
			if err := c.Run(); err != nil {
				return nil, fmt.Errorf("kubectl %s: %w: %s", firstNonFlag(args), err, stderr.String())
			}
			return stdout.Bytes(), nil
		},
	}
}

// firstNonFlag names the kubectl verb in an error without echoing any other arg.
func firstNonFlag(args []string) string {
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--context", "-n":
			i++
		default:
			return args[i]
		}
	}
	return ""
}

func runCodexLogin(cmd *cobra.Command, _ []string) error {
	return codexLogin(cmd.Context(), codexLoginFlags.secret, codexLoginFlags.namespace,
		codexLoginFlags.kubeContext, codexLoginFlags.codexBin, codexLoginFlags.kubectlBin,
		defaultCodexLoginDeps(), cmd.OutOrStdout(), cmd.ErrOrStderr())
}

func codexLogin(ctx context.Context, secret, ns, kubeCtx, codexBin, kubectlBin string, d codexLoginDeps, stdout, stderr io.Writer) (err error) {
	if secret == "" {
		return errors.New("--secret is required")
	}
	home, err := os.MkdirTemp("", "gb-codex-login-")
	if err != nil {
		return fmt.Errorf("creating throwaway CODEX_HOME: %w", err)
	}
	defer func() {
		if rerr := os.RemoveAll(home); rerr != nil && err == nil {
			err = fmt.Errorf("removing throwaway CODEX_HOME %s: %w", home, rerr)
		}
	}()

	fmt.Fprintln(stderr, "Approve the code below in a browser signed in as the ChatGPT plan owner.")
	if err := d.runCodex(ctx, codexBin, home, stdout, stderr); err != nil {
		return fmt.Errorf("codex login --device-auth: %w", err)
	}

	doc, err := os.ReadFile(filepath.Join(home, "auth.json"))
	if err != nil {
		return fmt.Errorf("codex login finished but wrote no auth.json: %w", err)
	}
	a, err := codexauth.Parse(doc)
	if err != nil {
		return fmt.Errorf("login result is not a ChatGPT-plan login: %w", err)
	}

	kube := func(extra ...string) []string {
		var args []string
		if kubeCtx != "" {
			args = append(args, "--context", kubeCtx)
		}
		if ns != "" {
			args = append(args, "-n", ns)
		}
		return append(args, extra...)
	}

	// Render the Secret manifest client-side and apply it: the document only ever
	// travels via a file in the throwaway home and kubectl's stdin, never on a
	// command line or to the terminal.
	manifest, err := d.kubectl(ctx, kubectlBin, kube("create", "secret", "generic", secret,
		"--from-file=auth.json="+filepath.Join(home, "auth.json"), "--dry-run=client", "-o", "yaml"), nil)
	if err != nil {
		return err
	}
	if _, err := d.kubectl(ctx, kubectlBin, kube("apply", "-f", "-"), manifest); err != nil {
		return err
	}
	fmt.Fprintf(stderr, "Stored login in Secret %q (refresh fingerprint %s). The keeper will pick it up on its next pass.\n",
		secret, a.RefreshFingerprint())
	return nil
}
