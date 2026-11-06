package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/steward"

	"github.com/spf13/cobra"
)

// gb lands digest is the steward's daily digest (spec §4.2 item 4, step
// 9.6): it reads what changed in the land since the last digest, or says the
// day was quiet and nothing is posted; --post says the steward's digest in
// #general and records where the next one starts.

var landsDigestCmd = &cobra.Command{
	Use:   "digest <land>",
	Short: "What changed in a land since the last digest; --post says the digest in #general",
	Long: `With no flags, prints what changed in the land since the last digest (or a
day back before the first): the steps closed (their pull requests merged), the
plans written, finished or dropped, the decisions answered, the gardener
sessions started; then where it stands: the steps in progress, the In flight
section of the steward's notes, the credits spent since and remaining, and
what the land's people must do: the decisions waiting on a person, the
owner-audience escalations and the owner-at-device steps, grouped by place
(GitHub, Slack admin, Keycloak, AWS console, phone, a device), each
with its clicks (errand_clicks) and what closes it (errand_evidence). It
writes nothing.

A quiet day (nothing done, planned, answered or run, no new errand) prints
one line: post nothing.

--post <file> (or - for stdin) says the file in the land's #general and
records --as-of (and --used, the used credits then) on the steward's notes
bead, where the next digest starts. Pass the --as-of and --used the read
printed, so nothing between the read and the post is missed. A digest as of
that time or later already posted is refused, so a retried post is not said
twice.`,
	Example: `  gb lands digest tomato-patch
  gb lands digest tomato-patch --post digest.md --as-of 2026-10-23T08:13:00Z --used 12.40`,
	Args: cobra.ExactArgs(1),
	RunE: runLandsDigest,
}

func runLandsDigest(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	land := args[0]
	file, _ := cmd.Flags().GetString("post")
	asOfFlag, _ := cmd.Flags().GetString("as-of")
	usedFlag, _ := cmd.Flags().GetString("used")

	if file == "" {
		if asOfFlag != "" || usedFlag != "" {
			return errors.New("--as-of and --used go with --post")
		}
		now := time.Now()
		p := steward.ReadPurse(ctx, daemon, land, now)
		d, err := steward.GatherDigest(ctx, daemon, land, p.CreditsOnly(), now)
		if err != nil {
			return err
		}
		if jsonOutput {
			out := struct {
				*steward.Digest
				Post string `json:"post,omitempty"`
			}{Digest: d}
			if !d.Quiet {
				out.Post = d.PostCommand()
			}
			return json.NewEncoder(os.Stdout).Encode(out)
		}
		fmt.Print(steward.FormatDigest(d))
		return nil
	}

	if asOfFlag == "" {
		return errors.New("--post needs --as-of: the as-of time the digest read printed")
	}
	asOf, err := time.Parse(time.RFC3339, asOfFlag)
	if err != nil {
		return fmt.Errorf("--as-of %q is not RFC 3339", asOfFlag)
	}
	if asOf.After(time.Now().Add(5 * time.Minute)) {
		return fmt.Errorf("--as-of %s is in the future", asOfFlag)
	}
	var used *float64
	if usedFlag != "" {
		f, err := strconv.ParseFloat(usedFlag, 64)
		if err != nil || f < 0 {
			return fmt.Errorf("--used %q is not a credit figure", usedFlag)
		}
		used = &f
	}
	var raw []byte
	if file == "-" {
		raw, err = io.ReadAll(os.Stdin)
	} else {
		raw, err = os.ReadFile(file)
	}
	if err != nil {
		return fmt.Errorf("reading %s: %w", file, err)
	}
	text := strings.TrimSpace(string(raw))
	if text == "" {
		return fmt.Errorf("%s is empty; on a quiet day post nothing", file)
	}

	notes, err := steward.CheckDigestMark(ctx, daemon, land, asOf)
	if err != nil {
		return err
	}
	bridgeURL := resolveBridgeURL()
	if bridgeURL == "" {
		return errors.New("SLACK_BRIDGE_URL is not set — cannot reach the bridge")
	}
	p := newSayParams(text)
	p.Channel = "general"
	if p.Land == "" {
		p.Land = land
	}
	said, err := conversationSay(ctx, bridgeURL, p, nil)
	if err != nil {
		return fmt.Errorf("saying the digest in %s's #general: %w", land, err)
	}
	if err := steward.RecordDigest(ctx, daemon, notes.ID, asOf, used); err != nil {
		fmt.Fprintf(os.Stderr, "WARNING: the digest is said (%s) but where the next one starts is not recorded: %v\nDo not post it again.\n", said.ID, err)
		return err
	}
	if jsonOutput {
		return json.NewEncoder(os.Stdout).Encode(map[string]string{"land": land, "message": said.ID, "as_of": asOf.UTC().Format(time.RFC3339)})
	}
	fmt.Printf("said the digest in %s's #general (%s); the next digest starts at %s\n", land, said.ID, asOf.UTC().Format(time.RFC3339))
	return nil
}

func init() {
	landsDigestCmd.Flags().String("post", "", "the digest to say in #general (- for stdin)")
	landsDigestCmd.Flags().String("as-of", "", "the as-of time the read printed (RFC 3339), with --post")
	landsDigestCmd.Flags().String("used", "", "the used credits the read printed, with --post")
	landsCmd.AddCommand(landsDigestCmd)
}
