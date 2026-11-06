package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// slackViewScript is the Playwright script that does the capture. It is a
// byte-for-byte copy of tools/slack-view/slack-view.mjs (the agent image carries
// only the gb binary, not the repo's tools/ dir); TestSlackViewScriptInSync keeps
// the two from drifting.
//
//go:embed slack_view.mjs
var slackViewScript []byte

// exitCodeError makes main exit with a specific code. slack view passes the
// capture script's own codes through (3 = signed out, 4 = no messages) so a
// caller can tell "refresh the session" from "the page is empty".
type exitCodeError struct {
	code int
	msg  string
}

func (e *exitCodeError) Error() string { return e.msg }

var slackViewCmd = &cobra.Command{
	Use:   "view <channel|permalink>",
	Short: "Screenshot a Slack channel or thread as the test user",
	Long: `Opens a Slack channel or thread in the web client as the Slack test user and
captures what renders: desktop (1280x800) and mobile (390x844) screenshots, each
in light and dark, plus the visible message text as JSON.

<target> is a channel id (C…, needs --team), <team>/<channel>, a channel URL
(https://app.slack.com/client/<team>/<channel>) or a message/thread permalink.

Output goes to --out (default ./slack-view-out/<UTC timestamp>):
  desktop-light.png desktop-dark.png mobile-light.png mobile-dark.png messages.json
stdout lists them; with --json it is the messages.json document plus outDir,
messagesPath and an absolute path on each screenshot. Progress goes to stderr.

Needs the test user's session: --storage-state, or $SLACK_VIEW_STORAGE_STATE
(set on pods that have it), or $SLACK_STORAGE_STATE. The file holds cookies; keep
it out of --out. Needs node and playwright (both in the agent image).

Exit codes: 0 ok, 2 usage, 3 signed out (session expired), 4 no messages
appeared within --timeout, 1 anything else.`,
	Example: `  gb slack view T0ABCDEF1/C0123456789 --out /tmp/shots
  gb slack view https://ws.slack.com/archives/C0123456789/p1791936599928719 --team T0ABCDEF1 --json`,
	Args: cobra.ExactArgs(1),
	RunE: runSlackView,
}

func init() {
	f := slackViewCmd.Flags()
	f.String("storage-state", "", "Playwright storageState JSON for the test user (default $SLACK_VIEW_STORAGE_STATE, then $SLACK_STORAGE_STATE)")
	f.String("out", "", "output directory (default ./slack-view-out/<UTC timestamp>)")
	f.String("team", "", "Slack team id, for channel-id targets and thread permalinks (default $SLACK_TEAM_ID)")
	f.Duration("timeout", 30*time.Second, "how long to wait for messages to appear")
	f.Duration("settle", 1500*time.Millisecond, "extra wait after messages appear, for images and emoji")
	f.Bool("full-page", false, "full-page screenshots instead of the viewport")
	f.String("message-selector", "", "override the message container CSS selector")
	f.String("scope-selector", "", "restrict text extraction to this CSS subtree")
	f.String("script", "", "run this slack-view.mjs instead of the built-in copy (also $GB_SLACK_VIEW_SCRIPT)")
	f.Bool("json", false, "print the messages.json document (with absolute screenshot paths) instead of a summary")
	slackCmd.AddCommand(slackViewCmd)
}

// slackViewNotProvisionedMsg is what `gb slack view` prints when the test user's
// session is absent: the pod mounts the Secret optionally, so an unprovisioned
// one is an expected state, not a crash.
const slackViewNotProvisionedMsg = "Slack viewer session not provisioned: no storageState file for the Slack test user " +
	"(pass --storage-state or set " + slackViewStorageStateEnv + "; on agent pods the operator provisions Secret slack-e2e-session, " +
	"see gardenio/docs/slack-e2e-provisioning.md)"

func runSlackView(cmd *cobra.Command, args []string) error {
	fl := cmd.Flags()
	str := func(name string) string { v, _ := fl.GetString(name); return v }
	asJSON, _ := fl.GetBool("json")
	fullPage, _ := fl.GetBool("full-page")
	timeout, _ := fl.GetDuration("timeout")
	settle, _ := fl.GetDuration("settle")

	storage := firstNonEmpty(str("storage-state"), os.Getenv(slackViewStorageStateEnv), os.Getenv("SLACK_STORAGE_STATE"))
	if storage == "" {
		return &exitCodeError{2, slackViewNotProvisionedMsg}
	}
	if _, err := os.Stat(storage); err != nil {
		return &exitCodeError{2, fmt.Sprintf("%s (cannot read %s: %v)", slackViewNotProvisionedMsg, storage, err)}
	}
	out := str("out")
	if out == "" {
		out = filepath.Join("slack-view-out", time.Now().UTC().Format("20060102T150405Z"))
	}
	out, err := filepath.Abs(out)
	if err != nil {
		return err
	}

	node := firstNonEmpty(os.Getenv("GB_NODE"), "node")
	if _, err := exec.LookPath(node); err != nil {
		return fmt.Errorf("node not found on PATH (slack view runs a Playwright script): %w", err)
	}
	script := firstNonEmpty(str("script"), os.Getenv("GB_SLACK_VIEW_SCRIPT"))
	if script == "" {
		dir, err := os.MkdirTemp("", "gb-slack-view-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(dir)
		script = filepath.Join(dir, "slack-view.mjs")
		if err := os.WriteFile(script, slackViewScript, 0o600); err != nil {
			return err
		}
	}

	argv := []string{script, "--storage-state", storage, "--out", out,
		"--timeout", strconv.FormatInt(timeout.Milliseconds(), 10),
		"--settle", strconv.FormatInt(settle.Milliseconds(), 10)}
	if fullPage {
		argv = append(argv, "--full-page")
	}
	for _, o := range []string{"team", "message-selector", "scope-selector"} {
		if v := str(o); v != "" {
			argv = append(argv, "--"+o, v)
		}
	}
	argv = append(argv, args[0])

	// Allow the page wait plus browser start-up and the four screenshots.
	parent := cmd.Context()
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, timeout+settle+2*time.Minute)
	defer cancel()
	c := exec.CommandContext(ctx, node, argv...)
	var stdout bytes.Buffer
	c.Stdout = &stdout
	c.Stderr = cmd.ErrOrStderr()
	if err := c.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() > 0 {
			return &exitCodeError{ee.ExitCode(), fmt.Sprintf("slack-view failed (exit %d): %s", ee.ExitCode(), slackViewExitHint(ee.ExitCode()))}
		}
		return fmt.Errorf("running slack-view: %w", err)
	}

	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	jsonPath := strings.TrimSpace(lines[len(lines)-1])
	return printSlackViewResult(cmd.OutOrStdout(), jsonPath, out, asJSON)
}

func slackViewExitHint(code int) string {
	switch code {
	case 2:
		return "bad arguments"
	case 3:
		return "not signed in; the test user's storageState is missing or expired (see gardenio/docs/slack-e2e-provisioning.md)"
	case 4:
		return "no messages appeared before --timeout; check the target, or the message selector"
	}
	return "see the output above"
}

func printSlackViewResult(w io.Writer, jsonPath, out string, asJSON bool) error {
	raw, err := os.ReadFile(jsonPath)
	if err != nil {
		return fmt.Errorf("slack-view did not write messages.json: %w", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("parsing %s: %w", jsonPath, err)
	}
	shots, _ := doc["screenshots"].([]any)
	var paths []string
	for _, s := range shots {
		if m, ok := s.(map[string]any); ok {
			if f, _ := m["file"].(string); f != "" {
				p := filepath.Join(out, f)
				m["path"] = p
				paths = append(paths, p)
			}
		}
	}
	doc["outDir"] = out
	doc["messagesPath"] = jsonPath

	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(doc)
	}
	msgs, _ := doc["messages"].([]any)
	fmt.Fprintf(w, "captured %d message(s) from %v\n", len(msgs), doc["url"])
	for _, p := range paths {
		fmt.Fprintln(w, p)
	}
	fmt.Fprintln(w, jsonPath)
	return nil
}
