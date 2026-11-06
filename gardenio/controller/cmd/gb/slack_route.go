package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/spf13/cobra"
)

// This file adds `gb slack route <agent|bead>` (gd-XahY2dfWYP): which Slack
// channel the bridge would post an agent's card, decisions and escalations to,
// and which routing rule chose it. It reads GET /api/slack/route, which runs
// the bridge's own resolveChannel rules, so nobody has to read bridge code to
// learn why a card went somewhere.

var slackRouteCmd = &cobra.Command{
	Use:   "route <agent|bead>",
	Short: "Explain which Slack channel an agent's cards go to, and why",
	Long: `Prints the Slack channel the bridge would use for an agent, the routing rule
that chose it, and the rules that lost. The rules, highest first:

  router                   router override or agent pattern
  spawn_channel            explicit pin: /spawn channel, --slack-spawn-channel,
                           a Slack-bound task's channel, or a schedule channel
                           (a spawn channel inherited from a same-role spawner)
  role_channel             the project's channel_roles entry for the agent's role
  inherited_spawn_channel  a spawn channel inherited from a different-role spawner
  project_primary          the project's first slack_channel
  router_default           the router's default channel
  bot_default              the bridge's default channel

The argument is an agent name, or a bead id: an agent bead, a decision (its
assignee), an escalation (the agent that filed it) or any bead (its assignee).
Also says whether the agent is thread-bound and where its card actually is,
which can differ from where the rules point (a thread-bound agent's card lives
in its thread; an older card stays where it was posted).

Only requires SLACK_BRIDGE_URL.

Examples:
  gb slack route gardenio-gb-slack-route-w9r
  gb slack route gd-XahY2dfWYP
  gb slack route gd-XahY2dfWYP --json`,
	Args: cobra.ExactArgs(1),
	RunE: runSlackRoute,
}

// routeBeadIDPattern matches a bead id; anything else is an agent name.
var routeBeadIDPattern = regexp.MustCompile(`^(?:gd|ka)-[A-Za-z0-9]+$`)

// slackRouteCandidate mirrors bridge.RouteCandidate.
type slackRouteCandidate struct {
	Rule        string `json:"rule"`
	Channel     string `json:"channel,omitempty"`
	ChannelName string `json:"channel_name,omitempty"`
	Applies     bool   `json:"applies"`
	Won         bool   `json:"won,omitempty"`
	Detail      string `json:"detail"`
}

// slackRouteResponse mirrors the bridge's GET /api/slack/route body.
type slackRouteResponse struct {
	Agent             string                `json:"agent"`
	Channel           string                `json:"channel"`
	ChannelName       string                `json:"channel_name,omitempty"`
	Rule              string                `json:"rule"`
	Project           string                `json:"project,omitempty"`
	Role              string                `json:"role,omitempty"`
	SpawnChannel      string                `json:"spawn_channel,omitempty"`
	InheritedFromRole string                `json:"inherited_from_role,omitempty"`
	Candidates        []slackRouteCandidate `json:"candidates"`
	Bead              string                `json:"bead,omitempty"`
	BeadType          string                `json:"bead_type,omitempty"`
	CacheRefreshed    bool                  `json:"cache_refreshed,omitempty"`
	ThreadBound       bool                  `json:"thread_bound"`
	CardFound         bool                  `json:"card_found"`
	CardChannel       string                `json:"card_channel,omitempty"`
	CardChannelName   string                `json:"card_channel_name,omitempty"`
	CardTS            string                `json:"card_ts,omitempty"`
}

func runSlackRoute(cmd *cobra.Command, args []string) error {
	bridgeURL := resolveBridgeURL()
	if bridgeURL == "" {
		return fmt.Errorf("SLACK_BRIDGE_URL is not set — cannot reach the Slack bridge")
	}
	target := strings.TrimSpace(args[0])
	key := "agent"
	if routeBeadIDPattern.MatchString(target) {
		key = "bead"
	}
	u := strings.TrimRight(bridgeURL, "/") + "/api/slack/route?" + url.Values{key: {target}}.Encode()

	resp, err := slackHTTPClient().Get(u)
	if err != nil {
		return fmt.Errorf("fetching route: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("reading route: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(body, &e) == nil && e.Error != "" {
			return fmt.Errorf("bridge: %s", e.Error)
		}
		if resp.StatusCode == http.StatusNotFound {
			return fmt.Errorf("bridge returned HTTP 404 — it predates gb slack route")
		}
		return fmt.Errorf("bridge returned HTTP %d", resp.StatusCode)
	}

	var route slackRouteResponse
	if err := json.Unmarshal(body, &route); err != nil {
		return fmt.Errorf("decoding route: %w", err)
	}
	if jsonOutput {
		printJSON(route)
		return nil
	}
	fmt.Fprint(cmd.OutOrStdout(), formatSlackRoute(route))
	return nil
}

// routeChannelLabel renders a channel as "#name (ID)", or the bare ID when the
// bridge could not read its name.
func routeChannelLabel(id, name string) string {
	switch {
	case id == "":
		return "(none)"
	case name == "":
		return id
	default:
		return "#" + name + " (" + id + ")"
	}
}

func formatSlackRoute(r slackRouteResponse) string {
	var sb strings.Builder
	agent := r.Agent
	if r.Bead != "" {
		agent += fmt.Sprintf("  (from %s %s)", r.BeadType, r.Bead)
	}
	fmt.Fprintf(&sb, "Agent:    %s\n", agent)
	fmt.Fprintf(&sb, "Channel:  %s\n", routeChannelLabel(r.Channel, r.ChannelName))
	rule := r.Rule
	for _, c := range r.Candidates {
		if c.Won {
			rule += " — " + c.Detail
		}
	}
	fmt.Fprintf(&sb, "Rule:     %s\n", rule)

	inputs := []string{"project=" + orNone(r.Project), "role=" + orNone(r.Role), "spawn_channel=" + orNone(r.SpawnChannel)}
	if r.InheritedFromRole != "" {
		inputs = append(inputs, "inherited_from_role="+r.InheritedFromRole)
	}
	fmt.Fprintf(&sb, "Inputs:   %s", strings.Join(inputs, " "))
	if r.CacheRefreshed {
		sb.WriteString("  (reloaded from the agent bead)")
	}
	sb.WriteString("\n")

	threadBound := "no"
	if r.ThreadBound {
		threadBound = "yes — its card lives in its Slack thread"
	}
	fmt.Fprintf(&sb, "Thread:   %s\n", threadBound)
	if r.CardFound {
		fmt.Fprintf(&sb, "Card:     %s ts %s\n", routeChannelLabel(r.CardChannel, r.CardChannelName), r.CardTS)
		if r.CardChannel != r.Channel {
			sb.WriteString("          the card is not in the routed channel (posted before routing changed, or thread-bound)\n")
		}
	} else {
		sb.WriteString("Card:     none recorded\n")
	}

	sb.WriteString("\nCandidates (highest precedence first):\n")
	for _, c := range r.Candidates {
		mark := "-   "
		switch {
		case c.Won:
			mark = "won "
		case c.Applies:
			mark = "lost"
		}
		ch := ""
		if c.Channel != "" {
			ch = routeChannelLabel(c.Channel, c.ChannelName) + "  "
		}
		fmt.Fprintf(&sb, "  %s  %-24s %s%s\n", mark, c.Rule, ch, c.Detail)
	}
	return sb.String()
}

func init() {
	slackCmd.AddCommand(slackRouteCmd)
}
