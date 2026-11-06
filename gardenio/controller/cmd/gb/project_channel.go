package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"

	"gardenio/controller/internal/beadsapi"

	"github.com/spf13/cobra"
)

// This file adds `gb project channel add|rm|list`: wiring a Slack channel to a
// project in one command. The Slack bridge reads three fields off the
// type:project bead to route a channel (beadsapi.ListProjectBeads):
//
//   - slack_channel  — CSV of channel IDs the project owns; the FIRST is the
//     project's primary channel.
//   - channel_roles  — JSON map {channelID: "role[,role…]"}: the role an
//     @mention spawns and whose cards route back to the channel.
//   - channel_modes  — JSON map {channelID: "mention"|"concierge"}.
//
// Before this, operators hand-wrote several `kd update --field-append /
// --field-set` calls and a bare `-f` clobbered the shared map. Here the three
// fields are recomputed from a fresh read of the bead and written back in ONE
// PATCH carrying only those three keys, so the bead never shows a channel half
// wired. This is not `gb channel`, which manages Lands Commons type=channel
// beads.

// validChannelModes are the interaction modes the bridge distinguishes
// (ProjectInfo.ChannelMode); any other value would silently behave as mention.
var validChannelModes = []string{"mention", "concierge"}

var projectChannelCmd = &cobra.Command{
	Use:   "channel",
	Short: "Wire Slack channels to a project (slack_channel, channel_roles, channel_modes)",
	Long: `Edit the Slack channel wiring on a type:project bead: the slack_channel CSV
and the channel_roles / channel_modes maps the Slack bridge reads. Each add or
rm rewrites all three fields together in one bead update, so the other
channels on the project are kept.

The project defaults to KD_PROJECT, then BOAT_PROJECT; pass --project to pick
another.

  gb project channel add #gardenio-ops --role crew --mode concierge
  gb project channel add C0XXXXXXXXX --primary
  gb project channel rm C0XXXXXXXXX
  gb project channel list

This is not 'gb channel', which manages Lands Commons channel beads.`,
}

// project-channel flags.
var (
	projectChannelProject string
	projectChannelRole    string
	projectChannelMode    string
	projectChannelPrimary bool
)

var projectChannelAddCmd = &cobra.Command{
	Use:   "add <channel>",
	Short: "Wire a Slack channel to the project, optionally with a role and mode",
	Long: `Add a Slack channel to the project's slack_channel list and, with --role or
--mode, set its channel_roles / channel_modes entries, all in one bead update.

The channel may be an ID (C…/G…) or a name (resolved through the Slack bridge,
which must be a member of the channel). The command refuses a channel another
project already claims, and warns when a --role has no type:role bead.

Re-running add on a wired channel is safe: it only changes what you pass. A
channel keeps its place in slack_channel unless you pass --primary, which moves
it first (the project's primary channel).

  gb project channel add #bill-navarro --role crew
  gb project channel add C0XXXXXXXXX --role monorepo-repro,monorepo-fix --mode concierge`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		channelID, err := resolveProjectChannelArg(args[0])
		if err != nil {
			return err
		}
		return runProjectChannelAdd(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), projectChannelAddOpts{
			Project: resolveProject(projectChannelProject),
			Channel: channelID,
			Role:    projectChannelRole,
			Mode:    projectChannelMode,
			Primary: projectChannelPrimary,
		})
	},
}

var projectChannelRmCmd = &cobra.Command{
	Use:     "rm <channel>",
	Aliases: []string{"remove"},
	Short:   "Unwire a Slack channel: drop its slack_channel, channel_roles and channel_modes entries",
	Args:    cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		channelID, err := resolveProjectChannelArg(args[0])
		if err != nil {
			return err
		}
		return runProjectChannelRm(cmd.Context(), cmd.OutOrStdout(), resolveProject(projectChannelProject), channelID)
	},
}

var projectChannelListCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls"},
	Short:   "List the project's Slack channels with their role, mode and primary flag",
	Args:    cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runProjectChannelList(cmd.Context(), cmd.OutOrStdout(), resolveProject(projectChannelProject))
	},
}

// resolveProjectChannelArg turns the <channel> argument into a channel ID. An
// ID passes through without touching the bridge, so wiring by ID works where
// SLACK_BRIDGE_URL is unset; only a name needs the bridge to resolve.
func resolveProjectChannelArg(arg string) (string, error) {
	arg = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(arg), "#"))
	if arg == "" {
		return "", fmt.Errorf("channel is required")
	}
	if strings.HasPrefix(arg, "C") || strings.HasPrefix(arg, "G") {
		return arg, nil
	}
	bridgeURL := resolveBridgeURL()
	if bridgeURL == "" {
		return "", fmt.Errorf("SLACK_BRIDGE_URL is not set — pass the channel ID (C…) instead of the name %q", arg)
	}
	return resolveChannelArg(bridgeURL, arg)
}

// channelWiring is the in-memory form of a project's three channel fields.
// Roles values keep the bead's comma-joined wire form ("role-a,role-b").
type channelWiring struct {
	Channels []string
	Roles    map[string]string
	Modes    map[string]string
}

// parseChannelWiring reads the three channel fields from a project bead's raw
// fields. A malformed channel_roles or channel_modes is an error, not an empty
// map: rewriting it from empty would drop every mapping on the bead.
func parseChannelWiring(fields map[string]string) (channelWiring, error) {
	w := channelWiring{
		Channels: splitCSV(fields["slack_channel"]),
		Roles:    map[string]string{},
		Modes:    map[string]string{},
	}
	for _, f := range []struct {
		name string
		out  *map[string]string
	}{{"channel_roles", &w.Roles}, {"channel_modes", &w.Modes}} {
		raw := strings.TrimSpace(fields[f.name])
		if raw == "" || raw == "null" {
			continue
		}
		if err := json.Unmarshal([]byte(raw), f.out); err != nil {
			return channelWiring{}, fmt.Errorf("%s on the project bead is not a JSON map (%v); fix it with kd before editing channels", f.name, err)
		}
		if *f.out == nil {
			*f.out = map[string]string{}
		}
	}
	return w, nil
}

// add wires channel into w. An empty role or mode leaves that entry as it is;
// primary moves the channel to the front of Channels. A new channel without
// primary is appended.
func (w *channelWiring) add(channel, role, mode string, primary bool) {
	idx := slices.Index(w.Channels, channel)
	switch {
	case primary:
		if idx >= 0 {
			w.Channels = slices.Delete(w.Channels, idx, idx+1)
		}
		w.Channels = slices.Insert(w.Channels, 0, channel)
	case idx < 0:
		w.Channels = append(w.Channels, channel)
	}
	if role != "" {
		w.Roles[channel] = role
	}
	if mode != "" {
		w.Modes[channel] = mode
	}
}

// remove drops every trace of channel from w and reports whether anything
// was there.
func (w *channelWiring) remove(channel string) bool {
	before := len(w.Channels)
	w.Channels = slices.DeleteFunc(w.Channels, func(c string) bool { return c == channel })
	_, hadRole := w.Roles[channel]
	_, hadMode := w.Modes[channel]
	delete(w.Roles, channel)
	delete(w.Modes, channel)
	return len(w.Channels) != before || hadRole || hadMode
}

// fields renders w as the PATCH body for the three fields. The maps go out as
// native JSON objects, the shape `kd update --field-set` also writes.
func (w channelWiring) fields() map[string]any {
	return map[string]any{
		"slack_channel": strings.Join(w.Channels, ","),
		"channel_roles": w.Roles,
		"channel_modes": w.Modes,
	}
}

// normalizeRoleList trims a --role value ("a, b") to the comma-joined wire form
// ParseChannelRolesMap reads ("a,b") and returns the individual roles.
func normalizeRoleList(raw string) (string, []string) {
	roles := splitCSV(raw)
	return strings.Join(roles, ","), roles
}

// channelOwnersExcept returns the other projects that already list channel in
// their slack_channel, sorted. It asks beadsapi.ChannelConflicts — the check
// the bridge's conflict audit uses — with project shown as owning channel.
func channelOwnersExcept(projects map[string]beadsapi.ProjectInfo, project, channel string) []string {
	probe := make(map[string]beadsapi.ProjectInfo, len(projects)+1)
	for name, info := range projects {
		probe[name] = info
	}
	self := probe[project]
	self.SlackChannels = append(slices.Clone(self.SlackChannels), channel)
	probe[project] = self
	return slices.DeleteFunc(slices.Clone(beadsapi.ChannelConflicts(probe)[channel]), func(n string) bool { return n == project })
}

// loadProjectChannelWiring finds the project bead and reads its channel fields
// from a fresh GET of the bead (not the list cache), which is the read side of
// the single read-modify-write PATCH that add and rm make.
func loadProjectChannelWiring(ctx context.Context, project string) (map[string]beadsapi.ProjectInfo, beadsapi.ProjectInfo, channelWiring, error) {
	if project == "" {
		return nil, beadsapi.ProjectInfo{}, channelWiring{}, fmt.Errorf("no project: pass --project or set KD_PROJECT")
	}
	projects, err := daemon.ListProjectBeads(ctx)
	if err != nil {
		return nil, beadsapi.ProjectInfo{}, channelWiring{}, err
	}
	info, ok := projects[project]
	if !ok {
		return nil, beadsapi.ProjectInfo{}, channelWiring{}, fmt.Errorf("project %q not found", project)
	}
	bead, err := daemon.GetBead(ctx, info.BeadID)
	if err != nil {
		return nil, beadsapi.ProjectInfo{}, channelWiring{}, fmt.Errorf("reading project bead %s: %w", info.BeadID, err)
	}
	w, err := parseChannelWiring(bead.Fields)
	if err != nil {
		return nil, beadsapi.ProjectInfo{}, channelWiring{}, fmt.Errorf("project %q (%s): %w", project, info.BeadID, err)
	}
	return projects, info, w, nil
}

type projectChannelAddOpts struct {
	Project string
	Channel string
	Role    string
	Mode    string
	Primary bool
}

func runProjectChannelAdd(ctx context.Context, out, errOut io.Writer, o projectChannelAddOpts) error {
	if o.Channel == "" {
		return fmt.Errorf("channel is required")
	}
	if strings.Contains(o.Channel, ",") {
		return fmt.Errorf("channel %q must be a single channel ID", o.Channel)
	}
	role, roles := normalizeRoleList(o.Role)
	mode := strings.TrimSpace(o.Mode)
	if mode != "" && !slices.Contains(validChannelModes, mode) {
		return fmt.Errorf("--mode %q is not one of: %s", mode, strings.Join(validChannelModes, ", "))
	}

	projects, info, w, err := loadProjectChannelWiring(ctx, o.Project)
	if err != nil {
		return err
	}

	// The bridge resolves a channel to ONE project; a channel listed on two
	// projects routes to whichever it meets first.
	if owners := channelOwnersExcept(projects, o.Project, o.Channel); len(owners) > 0 {
		return fmt.Errorf("channel %s is already wired to project %s; run 'gb project channel rm %s --project %s' first",
			o.Channel, strings.Join(owners, ", "), o.Channel, owners[0])
	}

	// A role with no type:role bead still spawns (with defaults), so this
	// warns rather than refuses. A failed role listing skips the check.
	if len(roles) > 0 {
		if known, err := daemon.ListRoleBeads(ctx); err != nil {
			fmt.Fprintf(errOut, "warning: could not list role beads to check --role: %v\n", err)
		} else {
			for _, r := range roles {
				if _, ok := known[r]; !ok {
					fmt.Fprintf(errOut, "warning: role %q has no type:role bead; an @mention in %s spawns it with default settings (create one with: kd create '%s' --type=role -l project:%s)\n", r, o.Channel, r, o.Project)
				}
			}
		}
	}

	w.add(o.Channel, role, mode, o.Primary)
	if err := daemon.UpdateBeadFieldsTyped(ctx, info.BeadID, w.fields()); err != nil {
		return err
	}

	fmt.Fprintf(out, "wired %s to project %s (%s)\n", o.Channel, o.Project, info.BeadID)
	printChannelWiringRow(out, w, o.Channel)
	return nil
}

func runProjectChannelRm(ctx context.Context, out io.Writer, project, channel string) error {
	_, info, w, err := loadProjectChannelWiring(ctx, project)
	if err != nil {
		return err
	}
	if !w.remove(channel) {
		fmt.Fprintf(out, "%s is not wired to project %s; nothing to do\n", channel, project)
		return nil
	}
	if err := daemon.UpdateBeadFieldsTyped(ctx, info.BeadID, w.fields()); err != nil {
		return err
	}
	fmt.Fprintf(out, "unwired %s from project %s (%s)\n", channel, project, info.BeadID)
	return nil
}

// projectChannelRow is one line of `list`, and its --json element.
type projectChannelRow struct {
	Channel string `json:"channel"`
	Role    string `json:"role,omitempty"`
	Mode    string `json:"mode,omitempty"`
	Primary bool   `json:"primary"`
	// Listed is false for a channel_roles/channel_modes key that is missing
	// from slack_channel: the bridge never routes it to this project.
	Listed bool `json:"listed"`
}

// channelRows lists every channel in w: slack_channel order first, then any
// map-only keys sorted.
func channelRows(w channelWiring) []projectChannelRow {
	var rows []projectChannelRow
	seen := map[string]bool{}
	for i, ch := range w.Channels {
		if seen[ch] {
			continue
		}
		seen[ch] = true
		rows = append(rows, projectChannelRow{Channel: ch, Role: w.Roles[ch], Mode: w.Modes[ch], Primary: i == 0, Listed: true})
	}
	var orphans []string
	for _, m := range []map[string]string{w.Roles, w.Modes} {
		for ch := range m {
			if !seen[ch] {
				seen[ch] = true
				orphans = append(orphans, ch)
			}
		}
	}
	sort.Strings(orphans)
	for _, ch := range orphans {
		rows = append(rows, projectChannelRow{Channel: ch, Role: w.Roles[ch], Mode: w.Modes[ch]})
	}
	return rows
}

func runProjectChannelList(ctx context.Context, out io.Writer, project string) error {
	_, info, w, err := loadProjectChannelWiring(ctx, project)
	if err != nil {
		return err
	}
	rows := channelRows(w)
	if jsonOutput {
		if rows == nil {
			rows = []projectChannelRow{}
		}
		printJSON(rows)
		return nil
	}
	if len(rows) == 0 {
		fmt.Fprintf(out, "project %s (%s) has no Slack channels wired\n", project, info.BeadID)
		return nil
	}
	fmt.Fprintf(out, "%-14s %-24s %s\n", "CHANNEL", "ROLE", "MODE")
	for _, r := range rows {
		fmt.Fprintln(out, formatChannelRow(r))
	}
	return nil
}

// formatChannelRow renders one list row. An unset mode shows as the bridge's
// default ("mention").
func formatChannelRow(r projectChannelRow) string {
	mode := r.Mode
	if mode == "" {
		mode = "mention"
	}
	note := ""
	switch {
	case r.Primary:
		note = "primary"
	case !r.Listed:
		note = "not in slack_channel (ignored)"
	}
	return strings.TrimRight(fmt.Sprintf("%-14s %-24s %-10s %s", r.Channel, orDash(r.Role), mode, note), " ")
}

// printChannelWiringRow prints channel's resulting row after an add.
func printChannelWiringRow(out io.Writer, w channelWiring, channel string) {
	for _, r := range channelRows(w) {
		if r.Channel == channel {
			fmt.Fprintf(out, "  %s\n", formatChannelRow(r))
			return
		}
	}
}

func init() {
	projectChannelCmd.PersistentFlags().StringVar(&projectChannelProject, "project", "", "project whose bead to edit (default: KD_PROJECT, then BOAT_PROJECT)")
	projectChannelAddCmd.Flags().StringVar(&projectChannelRole, "role", "", "role an @mention in the channel spawns (comma-separate several); unset keeps the current role")
	projectChannelAddCmd.Flags().StringVar(&projectChannelMode, "mode", "", "interaction mode: mention or concierge; unset keeps the current mode")
	projectChannelAddCmd.Flags().BoolVar(&projectChannelPrimary, "primary", false, "make this the project's primary channel (first in slack_channel)")

	projectChannelCmd.AddCommand(projectChannelAddCmd, projectChannelRmCmd, projectChannelListCmd)
	projectCmd.AddCommand(projectChannelCmd)
}
