package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"

	"github.com/spf13/cobra"
)

// gb channel seed creates a land's four default channels for an existing
// project and binds each to the Slack channel the project already routes that
// traffic to (Lands spec gardenio/docs/superpowers/specs/
// 2026-10-19-lands-commons-design.md §2.2 "Default channels", §7.1 step 3).
//
// Where each default channel routes today, read from the project bead and the
// bridge's SLACK_ROUTER_CONFIG:
//
//   - #general: the project's primary Slack channel (first slack_channel
//     entry), where humans @mention and thread agents' cards land.
//   - #crew: the channel the bridge's resolveChannel picks for a crew agent
//     with no spawn channel (bot_agents.go): a router pattern for
//     <land>/crew/*, else the channel_roles channel for "crew", else the
//     primary channel.
//   - #decisions, #escalations: decision and escalation cards post to
//     resolveChannel(agent) too, so they route where #crew does.
//
// slack_channel_id is unique across channel beads (the daemon enforces it),
// so one Slack channel binds to one channel bead: the first default channel
// in seedChannelOrder that routes to it. The others are created unbound with
// mirror=off and a note naming the channel that holds the binding. Re-bind
// later with kd update once a land has dedicated Slack channels.
//
// It also sets the land's two project-bead fields (§2.1) when they are unset:
// land_default_channel to the land's #general (the one it finds or the one it
// creates), and land_slack_mirror to "on" for a project that has a Slack
// channel, else "off". A field that already holds a value is left as it is.
//
// Idempotent: an existing open channel with a default name is left exactly as
// it is (reported, never modified), and so is a land field already set, so a
// re-run after an operator edit changes nothing. A dry run is the default;
// --apply writes.

// seedChannelOrder is both the creation order and the binding priority.
var seedChannelOrder = []string{"general", "crew", "decisions", "escalations"}

var (
	seedAllLands     bool
	seedApply        bool
	seedRouterConfig string
)

var channelSeedCmd = &cobra.Command{
	Use:   "seed",
	Short: "Create a land's four default channels, bound to its current Slack channels",
	Long: `Create #general, #crew, #decisions and #escalations in an existing project's
land, binding each to the Slack channel the project routes that traffic to
today (mirror=both). A Slack channel binds to one channel bead only, so when
several default channels route to the same Slack channel the first in the
order general, crew, decisions, escalations takes the binding and the rest are
created unbound (mirror=off).

It also sets the land's fields on the project bead when they are unset:
land_default_channel to the land's #general, and land_slack_mirror to "on"
when the project has a Slack channel, else "off". Fields already set are left
as they are.

Routing is read from the project bead (slack_channel, channel_roles,
channel_runtime_class, card_theme) and the bridge's SLACK_ROUTER_CONFIG
(--router-config, default $SLACK_ROUTER_CONFIG).

Dry run by default: prints the plan and writes nothing. --apply creates the
missing channels and sets the unset land fields. Existing channels and land
fields are never modified, so it is safe to re-run.`,
	Args: cobra.NoArgs,
	RunE: runChannelSeed,
}

// seedChannel is the plan for one default channel of one land.
type seedChannel struct {
	Land         string   `json:"land"`
	Name         string   `json:"name"`
	Action       string   `json:"action"` // create | exists | created | failed
	ID           string   `json:"id,omitempty"`
	SlackChannel string   `json:"slack_channel_id,omitempty"`
	Mirror       string   `json:"mirror,omitempty"`
	Roles        []string `json:"channel_roles,omitempty"`
	DefaultRole  string   `json:"default_role,omitempty"`
	RuntimeClass string   `json:"runtime_class,omitempty"`
	CardTheme    string   `json:"card_theme,omitempty"`
	Note         string   `json:"note,omitempty"`
}

// seedLandField is the plan for one land field on the project bead (§2.1).
type seedLandField struct {
	Land      string `json:"land"`
	ProjectID string `json:"project_id"`
	Field     string `json:"field"`
	Action    string `json:"action"` // set | exists | updated | failed
	Value     string `json:"value,omitempty"`
	Note      string `json:"note,omitempty"`
}

// seedResult is gb channel seed's --json output.
type seedResult struct {
	Channels   []seedChannel   `json:"channels"`
	LandFields []seedLandField `json:"land_fields"`
}

// seedRoute returns the Slack channel each default channel routes to today,
// before the one-binding-per-Slack-channel rule is applied ("" = none).
func seedRoute(p beadsapi.ProjectInfo, router *bridge.Router) map[string]string {
	primary := ""
	if len(p.SlackChannels) > 0 {
		primary = p.SlackChannels[0]
	}
	crew := primary
	if ch := p.ChannelForRole("crew"); ch != "" {
		crew = ch
	}
	if router != nil {
		// The probe name stands in for any crew agent: patterns are
		// project/role/name with "*" segments, and overrides are exact
		// agent names, which a made-up name never hits.
		if r := router.Resolve(p.Name + "/crew/" + "seed-probe"); r.ChannelID != "" && !r.IsDefault {
			crew = r.ChannelID
		}
	}
	return map[string]string{"general": primary, "crew": crew, "decisions": crew, "escalations": crew}
}

// planLandSeed builds the seed plan for one land. existing maps a name to the
// land's open channel bead of that name; bound maps a Slack channel id to the
// channel bead (in any land, open or archived) already bound to it.
func planLandSeed(p beadsapi.ProjectInfo, router *bridge.Router, existing map[string]*beadsapi.BeadDetail, bound map[string]string) []seedChannel {
	routes := seedRoute(p, router)
	// Slack channel id → name of the land's open channel mirroring it.
	claimedBy := map[string]string{}
	for name, ch := range existing {
		if id := ch.Fields["slack_channel_id"]; id != "" {
			claimedBy[id] = name
		}
	}
	theme := ""
	if p.CardTheme == "garden" || p.CardTheme == "business" {
		theme = p.CardTheme
	}
	var plan []seedChannel
	for _, name := range seedChannelOrder {
		sc := seedChannel{Land: p.Name, Name: name}
		slackID := routes[name]
		if ch := existing[name]; ch != nil {
			sc.Action, sc.ID = "exists", ch.ID
			sc.SlackChannel, sc.Mirror = ch.Fields["slack_channel_id"], ch.Fields["mirror"]
			if slackID != "" && slackID != sc.SlackChannel && claimedBy[slackID] == "" {
				sc.Note = fmt.Sprintf("left as is; Slack routing points at %s", slackID)
			}
			plan = append(plan, sc)
			continue
		}
		sc.Action, sc.Mirror = "create", "off"
		switch {
		case slackID == "":
			sc.Note = "project has no Slack channel"
		case claimedBy[slackID] != "":
			sc.Note = fmt.Sprintf("routes to %s, which #%s mirrors", slackID, claimedBy[slackID])
		case bound[slackID] != "":
			sc.Note = fmt.Sprintf("routes to %s, already bound to channel %s", slackID, bound[slackID])
		default:
			claimedBy[slackID] = name
			sc.SlackChannel, sc.Mirror = slackID, "both"
			sc.Roles = p.ChannelRoles[slackID]
			if len(sc.Roles) > 0 {
				sc.DefaultRole = sc.Roles[0]
			}
			sc.RuntimeClass = p.ChannelRuntimeClass(slackID)
			sc.CardTheme = theme
		}
		plan = append(plan, sc)
	}
	return plan
}

// planLandFields plans the land's project-bead fields. channels is the land's
// seed plan from planLandSeed. A land_default_channel to be set on a #general
// that is still to be created has no value until --apply creates it.
func planLandFields(p beadsapi.ProjectInfo, channels []seedChannel) []seedLandField {
	var general *seedChannel
	for i := range channels {
		if channels[i].Name == "general" {
			general = &channels[i]
		}
	}
	def := seedLandField{Land: p.Name, ProjectID: p.BeadID, Field: "land_default_channel"}
	switch {
	case p.LandDefaultChannel != "":
		def.Action, def.Value = "exists", p.LandDefaultChannel
		if general != nil && general.ID != "" && general.ID != p.LandDefaultChannel {
			def.Note = fmt.Sprintf("left as is; #general is %s", general.ID)
		}
	case general == nil:
		def.Action, def.Note = "exists", "no #general planned"
	case general.Action == "exists":
		def.Action, def.Value = "set", general.ID
	default:
		def.Action, def.Note = "set", "the #general this seed creates"
	}
	mirror := seedLandField{Land: p.Name, ProjectID: p.BeadID, Field: "land_slack_mirror"}
	switch {
	case p.LandSlackMirror != "":
		mirror.Action, mirror.Value = "exists", p.LandSlackMirror
	case len(p.SlackChannels) > 0:
		mirror.Action, mirror.Value = "set", "on"
	default:
		mirror.Action, mirror.Value, mirror.Note = "set", "off", "project has no Slack channel"
	}
	return []seedLandField{def, mirror}
}

// applyLandFields writes the planned land fields of every land in one patch
// per project bead, after the channels are created so a new #general's id is
// known. It returns how many fields failed.
func applyLandFields(ctx context.Context, fields []seedLandField, channels []seedChannel) int {
	generalID := map[string]string{}
	for _, sc := range channels {
		if sc.Name == "general" && (sc.Action == "exists" || sc.Action == "created") {
			generalID[sc.Land] = sc.ID
		}
	}
	byProject := map[string][]int{}
	var order []string
	failed := 0
	for i := range fields {
		lf := &fields[i]
		if lf.Action != "set" {
			continue
		}
		if lf.Field == "land_default_channel" && lf.Value == "" {
			if lf.Value = generalID[lf.Land]; lf.Value == "" {
				lf.Action, lf.Note = "failed", "#general was not created"
				failed++
				continue
			}
		}
		if byProject[lf.ProjectID] == nil {
			order = append(order, lf.ProjectID)
		}
		byProject[lf.ProjectID] = append(byProject[lf.ProjectID], i)
	}
	for _, id := range order {
		patch := map[string]string{}
		for _, i := range byProject[id] {
			patch[fields[i].Field] = fields[i].Value
		}
		err := daemon.UpdateBeadFields(ctx, id, patch)
		for _, i := range byProject[id] {
			if err != nil {
				fields[i].Action, fields[i].Note = "failed", err.Error()
				failed++
			} else {
				fields[i].Action = "updated"
			}
		}
	}
	return failed
}

// seedChannelRequest turns one planned channel into the bead create request.
func seedChannelRequest(sc seedChannel) (beadsapi.CreateBeadRequest, error) {
	fields := map[string]any{"name": sc.Name, "channel_kind": sc.Name, "mirror": sc.Mirror}
	for key, v := range map[string]string{
		"slack_channel_id": sc.SlackChannel,
		"default_role":     sc.DefaultRole,
		"runtime_class":    sc.RuntimeClass,
		"card_theme":       sc.CardTheme,
	} {
		if v != "" {
			fields[key] = v
		}
	}
	if len(sc.Roles) > 0 {
		fields["channel_roles"] = sc.Roles
	}
	raw, err := json.Marshal(fields)
	if err != nil {
		return beadsapi.CreateBeadRequest{}, fmt.Errorf("marshalling channel fields: %w", err)
	}
	topic := fmt.Sprintf("Default #%s channel of land %s, seeded by gb channel seed.", sc.Name, sc.Land)
	if sc.SlackChannel != "" {
		topic += fmt.Sprintf(" Mirrors Slack channel %s both ways.", sc.SlackChannel)
	}
	return beadsapi.CreateBeadRequest{
		Title:       "#" + sc.Name,
		Type:        "channel",
		Description: topic,
		CreatedBy:   actor,
		Labels:      []string{"project:" + sc.Land},
		Fields:      raw,
	}, nil
}

// seedLands returns the projects to seed: every open project with --all-lands,
// else the one named by --project / KD_PROJECT.
func seedLands(projects map[string]beadsapi.ProjectInfo) ([]beadsapi.ProjectInfo, error) {
	if seedAllLands {
		var out []beadsapi.ProjectInfo
		for _, p := range projects {
			if p.Status != "closed" {
				out = append(out, p)
			}
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
		return out, nil
	}
	land, err := channelLand()
	if err != nil {
		return nil, fmt.Errorf("%w, or --all-lands", err)
	}
	p, ok := projects[land]
	if !ok || p.Status == "closed" {
		return nil, fmt.Errorf("no open project %q to seed", land)
	}
	return []beadsapi.ProjectInfo{p}, nil
}

// seedRouter parses the bridge router config, or returns nil when unset.
func seedRouter(raw string) (*bridge.Router, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var cfg bridge.RouterConfig
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return nil, fmt.Errorf("parsing --router-config: %w", err)
	}
	return bridge.NewRouter(cfg), nil
}

// seedExisting reads every channel bead the caller can see and returns the
// open default-named channels per land and the Slack bindings of all of them.
func seedExisting(ctx context.Context) (map[string]map[string]*beadsapi.BeadDetail, map[string]string, error) {
	result, err := daemon.ListAllBeadsFiltered(ctx, beadsapi.ListBeadsQuery{Types: []string{"channel"}})
	if err != nil {
		return nil, nil, fmt.Errorf("listing channels: %w", err)
	}
	byLand := map[string]map[string]*beadsapi.BeadDetail{}
	bound := map[string]string{}
	for _, c := range result.Beads {
		if id := c.Fields["slack_channel_id"]; id != "" {
			bound[id] = c.ID
		}
		if c.Status == "closed" {
			continue
		}
		for _, l := range c.Labels {
			if land, ok := strings.CutPrefix(l, "project:"); ok {
				if byLand[land] == nil {
					byLand[land] = map[string]*beadsapi.BeadDetail{}
				}
				byLand[land][c.Fields["name"]] = c
			}
		}
	}
	return byLand, bound, nil
}

func runChannelSeed(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	router, err := seedRouter(seedRouterConfig)
	if err != nil {
		return err
	}
	projects, err := daemon.ListProjectBeads(ctx)
	if err != nil {
		return fmt.Errorf("listing projects: %w", err)
	}
	lands, err := seedLands(projects)
	if err != nil {
		return err
	}
	existing, bound, err := seedExisting(ctx)
	if err != nil {
		// A daemon older than the channel type (gd-D1hR1acddk) holds no
		// channels, so a dry run can still plan; a write cannot land.
		if seedApply || !strings.Contains(err.Error(), `unknown bead type "channel"`) {
			return err
		}
		fmt.Fprintln(os.Stderr, "warning: the daemon does not know type=channel yet; planning as if no channels exist (--apply needs a kbeads release with the channel type)")
	}
	var plan []seedChannel
	var landFields []seedLandField
	for _, p := range lands {
		landPlan := planLandSeed(p, router, existing[p.Name], bound)
		plan = append(plan, landPlan...)
		landFields = append(landFields, planLandFields(p, landPlan)...)
	}
	failed := 0
	if seedApply {
		for i := range plan {
			if plan[i].Action != "create" {
				continue
			}
			req, err := seedChannelRequest(plan[i])
			if err == nil {
				var bead *beadsapi.BeadDetail
				if bead, err = daemon.CreateBeadDetail(ctx, req); err == nil {
					plan[i].Action, plan[i].ID = "created", bead.ID
					continue
				}
			}
			plan[i].Action, plan[i].Note = "failed", err.Error()
			failed++
		}
		failed += applyLandFields(ctx, landFields, plan)
	}
	if jsonOutput {
		printJSON(seedResult{Channels: plan, LandFields: landFields})
	} else {
		printSeedPlan(os.Stdout, plan, landFields, seedApply)
	}
	if failed > 0 {
		return fmt.Errorf("%d write(s) failed", failed)
	}
	return nil
}

func printSeedPlan(w io.Writer, plan []seedChannel, landFields []seedLandField, applied bool) {
	if len(plan) == 0 {
		fmt.Fprintln(w, "No lands to seed.")
		return
	}
	fmt.Fprintf(w, "%-12s %-12s %-8s %-16s %-14s %-6s %s\n", "LAND", "CHANNEL", "ACTION", "ID", "SLACK", "MIRROR", "NOTE")
	pending := 0
	for _, sc := range plan {
		if sc.Action == "create" {
			pending++
		}
		fmt.Fprintf(w, "%-12s %-12s %-8s %-16s %-14s %-6s %s\n",
			sc.Land, "#"+sc.Name, sc.Action, orDash(sc.ID), orDash(sc.SlackChannel), orDash(sc.Mirror), sc.Note)
	}
	pendingFields := 0
	if len(landFields) > 0 {
		fmt.Fprintf(w, "\n%-12s %-20s %-8s %-16s %s\n", "LAND", "FIELD", "ACTION", "VALUE", "NOTE")
		for _, lf := range landFields {
			if lf.Action == "set" {
				pendingFields++
			}
			fmt.Fprintf(w, "%-12s %-20s %-8s %-16s %s\n", lf.Land, lf.Field, lf.Action, orDash(lf.Value), lf.Note)
		}
	}
	if !applied && pending+pendingFields > 0 {
		fmt.Fprintf(w, "\nDry run: %d channel(s) to create and %d land field(s) to set. Re-run with --apply to write them.\n", pending, pendingFields)
	}
}

func init() {
	f := channelSeedCmd.Flags()
	f.StringVar(&channelProject, "project", "", "land (project) to seed (default: KD_PROJECT / BOAT_PROJECT)")
	f.BoolVar(&seedAllLands, "all-lands", false, "seed every open project")
	f.BoolVar(&seedApply, "apply", false, "create the channels (default is a dry run)")
	f.StringVar(&seedRouterConfig, "router-config", os.Getenv("SLACK_ROUTER_CONFIG"), "the bridge's SLACK_ROUTER_CONFIG JSON")
	channelCmd.AddCommand(channelSeedCmd)
}
