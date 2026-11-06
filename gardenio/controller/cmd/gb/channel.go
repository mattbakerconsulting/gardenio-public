package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"gardenio/controller/internal/beadsapi"

	"github.com/spf13/cobra"
)

// channelCmd manages Commons channels: type=channel beads, one land (project)
// each (Lands spec gardenio/docs/superpowers/specs/2026-10-19-lands-commons-design.md
// §2.2). The daemon enforces the land rules (exactly one project: label, name
// unique per land, slack_channel_id unique across lands); this command only
// builds the bead and reports the daemon's refusal verbatim.
var channelCmd = &cobra.Command{
	Use:     "channel",
	Short:   "Manage Commons channels (type=channel beads) in a land",
	GroupID: "orchestration",
}

// channel-create flags.
var (
	channelProject      string
	channelKind         string
	channelTitle        string
	channelTopic        string
	channelRoles        []string
	channelDefaultRole  string
	channelRuntimeClass string
	channelCardTheme    string
	channelSlackID      string
	channelMirror       string
	channelMembers      []string
	channelListArchived bool
)

// defaultChannelKinds are the land's default channels (§2.2), and #machines
// (steward amendment 2026-10-23 §2): a channel with one of these names gets the
// same channel_kind unless --kind says otherwise.
var defaultChannelKinds = map[string]bool{"general": true, "crew": true, "decisions": true, "escalations": true, "machines": true}

var channelCreateCmd = &cobra.Command{
	Use:   "create <name>",
	Short: "Create a channel in a land",
	Long: `Create a type=channel bead in a land (project). <name> is the slug, unique
among the land's open channels.

--kind defaults to the name for general/crew/decisions/escalations/machines
and to "user" otherwise; a machines channel takes the land's machine posts
(agent cards, notices, desk escalations) and is never mirrored to Slack; a dm channel needs --kind dm and two or more --member
principals (oidc:<email> or agent:<name>). A user channel takes posts from its
members only: --member lists them, and without it you are the first member
(see gb channel join).`,
	Args: cobra.ExactArgs(1),
	RunE: runChannelCreate,
}

var channelListCmd = &cobra.Command{
	Use:   "list",
	Short: "List the channels of a land",
	Args:  cobra.NoArgs,
	RunE:  runChannelList,
}

var channelArchiveCmd = &cobra.Command{
	Use:   "archive <name|id>",
	Short: "Archive (close) a channel",
	Args:  cobra.ExactArgs(1),
	RunE:  runChannelArchive,
}

// channelLand is the land a channel command acts in: --project, then
// KD_PROJECT, then BOAT_PROJECT.
func channelLand() (string, error) {
	land := resolveProject(channelProject)
	if land == "" {
		return "", fmt.Errorf("no land: pass --project <name> (or set KD_PROJECT)")
	}
	return land, nil
}

// buildChannelBead turns the create flags into the bead request. Only fields
// the caller set are sent, so the daemon's own defaults stay authoritative.
func buildChannelBead(land, name string) (beadsapi.CreateBeadRequest, error) {
	kind := channelKind
	if kind == "" {
		kind = "user"
		if defaultChannelKinds[name] {
			kind = name
		}
	}
	fields := map[string]any{"name": name, "channel_kind": kind}
	for key, v := range map[string]string{
		"default_role":     channelDefaultRole,
		"runtime_class":    channelRuntimeClass,
		"card_theme":       channelCardTheme,
		"slack_channel_id": channelSlackID,
		"mirror":           channelMirror,
	} {
		if v != "" {
			fields[key] = v
		}
	}
	if len(channelRoles) > 0 {
		fields["channel_roles"] = channelRoles
	}
	if len(channelMembers) > 0 {
		fields["members"] = channelMembers
	} else if self := channelSelf(); kind == "user" && self != "" {
		// A user channel takes posts from its members only (channel_members.go),
		// so whoever creates one is its first member.
		fields["members"] = []string{self}
	}
	raw, err := json.Marshal(fields)
	if err != nil {
		return beadsapi.CreateBeadRequest{}, fmt.Errorf("marshalling channel fields: %w", err)
	}
	title := channelTitle
	if title == "" {
		title = "#" + name
	}
	return beadsapi.CreateBeadRequest{
		Title:       title,
		Type:        "channel",
		Description: channelTopic,
		CreatedBy:   actor,
		Labels:      []string{"project:" + land},
		Fields:      raw,
	}, nil
}

func runChannelCreate(cmd *cobra.Command, args []string) error {
	land, err := channelLand()
	if err != nil {
		return err
	}
	req, err := buildChannelBead(land, args[0])
	if err != nil {
		return err
	}
	bead, err := daemon.CreateBeadDetail(cmd.Context(), req)
	if err != nil {
		return fmt.Errorf("creating channel: %w", err)
	}
	if jsonOutput {
		printJSON(bead)
		return nil
	}
	fmt.Printf("Created channel %s (%s) in land %s\n", bead.Title, bead.ID, land)
	return nil
}

// listChannels returns the land's channels, open only unless archived is set.
func listChannels(ctx context.Context, land string, archived bool) ([]*beadsapi.BeadDetail, error) {
	q := beadsapi.ListBeadsQuery{
		Types:  []string{"channel"},
		Labels: []string{"project:" + land},
	}
	if !archived {
		q.Statuses = []string{"open", "in_progress", "blocked", "deferred", "partial"}
	}
	result, err := daemon.ListAllBeadsFiltered(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("listing channels: %w", err)
	}
	return result.Beads, nil
}

func runChannelList(cmd *cobra.Command, _ []string) error {
	land, err := channelLand()
	if err != nil {
		return err
	}
	channels, err := listChannels(cmd.Context(), land, channelListArchived)
	if err != nil {
		return err
	}
	if jsonOutput {
		printJSON(channels)
		return nil
	}
	printChannelTable(os.Stdout, land, channels)
	return nil
}

func printChannelTable(w io.Writer, land string, channels []*beadsapi.BeadDetail) {
	if len(channels) == 0 {
		fmt.Fprintf(w, "No channels in land %s.\n", land)
		return
	}
	fmt.Fprintf(w, "%-16s %-20s %-12s %-6s %-14s %-8s %s\n", "ID", "NAME", "KIND", "MIRROR", "SLACK", "STATUS", "TITLE")
	for _, c := range channels {
		status := "open"
		if c.Status == "closed" {
			status = "archived"
		}
		fmt.Fprintf(w, "%-16s %-20s %-12s %-6s %-14s %-8s %s\n",
			c.ID, c.Fields["name"], c.Fields["channel_kind"], orDash(c.Fields["mirror"]),
			orDash(c.Fields["slack_channel_id"]), status, c.Title)
	}
}

// resolveChannel finds an open channel in the land by name, or any channel by
// bead id. An id that names a bead of another type or land is refused.
func resolveChannel(ctx context.Context, land, ref string) (*beadsapi.BeadDetail, error) {
	channels, err := listChannels(ctx, land, true)
	if err != nil {
		return nil, err
	}
	var byName *beadsapi.BeadDetail
	for _, c := range channels {
		if c.ID == ref {
			return c, nil
		}
		if c.Fields["name"] == ref && c.Status != "closed" {
			byName = c
		}
	}
	if byName == nil {
		return nil, fmt.Errorf("no open channel %q in land %s (see gb channel list)", ref, land)
	}
	return byName, nil
}

func runChannelArchive(cmd *cobra.Command, args []string) error {
	land, err := channelLand()
	if err != nil {
		return err
	}
	ch, err := resolveChannel(cmd.Context(), land, args[0])
	if err != nil {
		return err
	}
	if ch.Status == "closed" {
		fmt.Printf("Channel %s (%s) is already archived\n", ch.Title, ch.ID)
		return nil
	}
	if err := daemon.CloseBeadAs(cmd.Context(), ch.ID, actor, nil); err != nil {
		return fmt.Errorf("archiving channel %s: %w", ch.ID, err)
	}
	fmt.Printf("Archived channel %s (%s) in land %s\n", ch.Title, ch.ID, land)
	return nil
}

func init() {
	for _, c := range []*cobra.Command{channelCreateCmd, channelListCmd, channelArchiveCmd} {
		c.Flags().StringVar(&channelProject, "project", "", "land (project) of the channel (default: KD_PROJECT / BOAT_PROJECT)")
	}
	f := channelCreateCmd.Flags()
	f.StringVar(&channelKind, "kind", "", "channel_kind: general|crew|decisions|escalations|machines|user|dm")
	f.StringVar(&channelTitle, "title", "", `display name (default "#<name>")`)
	f.StringVar(&channelTopic, "topic", "", "channel topic (the bead description)")
	f.StringSliceVar(&channelRoles, "roles", nil, "roles whose agent cards land here (channel_roles)")
	f.StringVar(&channelDefaultRole, "default-role", "", "role a plain @gardenio mention spawns here")
	f.StringVar(&channelRuntimeClass, "runtime-class", "", "runtime class for agents spawned here")
	f.StringVar(&channelCardTheme, "card-theme", "", "Slack card theme: garden|business")
	f.StringVar(&channelSlackID, "slack-channel", "", "mirrored Slack channel id (C…); admin only")
	f.StringVar(&channelMirror, "mirror", "", "Slack mirroring: off|out|both; admin only")
	f.StringArrayVar(&channelMembers, "member", nil, "member principal of a dm or user channel, oidc:<email> or agent:<name> (repeatable)")
	channelListCmd.Flags().BoolVar(&channelListArchived, "archived", false, "include archived channels")

	channelCmd.AddCommand(channelCreateCmd, channelListCmd, channelArchiveCmd)
	rootCmd.AddCommand(channelCmd)
}
