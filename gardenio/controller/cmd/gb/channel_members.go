package main

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/conversation"

	"github.com/spf13/cobra"
)

// Channel membership (Lands spec §7.3 step 4.8): a user channel or a DM takes
// posts only from its members (fields.members). Everyone in the land still
// reads it; private channels are LATER. The daemon decides who may change
// whom: a person joins or leaves alone, a land owner adds or removes anyone,
// and a guest only leaves. These commands build the new list and report the
// daemon's refusal verbatim.

// channelMemberFlag is --member on join and leave: someone other than the
// caller (a land owner's or the operator's move).
var channelMemberFlag string

var channelJoinCmd = &cobra.Command{
	Use:   "join <name|id>",
	Short: "Join a user channel (or add --member to it, as a land owner)",
	Long: `Add yourself (or --member) to a user channel's members. A user channel takes
posts only from its members; the land's default channels need no joining.

You are agent:<name> in an agent pod and oidc:<email> after gb login.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error { return runChannelMembership(cmd, args[0], true) },
}

var channelLeaveCmd = &cobra.Command{
	Use:   "leave <name|id>",
	Short: "Leave a user channel (or remove --member from it, as a land owner)",
	Args:  cobra.ExactArgs(1),
	RunE:  func(cmd *cobra.Command, args []string) error { return runChannelMembership(cmd, args[0], false) },
}

var channelMembersCmd = &cobra.Command{
	Use:   "members <name|id>",
	Short: "List a channel's members",
	Args:  cobra.ExactArgs(1),
	RunE:  runChannelMembers,
}

// channelSelf returns the caller's member principal, or "" when it cannot tell.
// A var so tests pin it.
var channelSelf = func() string {
	if os.Getenv("KD_AGENT_ID") != "" || os.Getenv("BOAT_AGENT_BEAD_ID") != "" {
		if actor != "" && actor != "unknown" {
			return "agent:" + actor
		}
	}
	return operatedByFromCredstore()
}

// channelMemberList reads fields.members, which the daemon returns as the JSON
// text of the array. Anything unreadable is no members.
func channelMemberList(ch *beadsapi.BeadDetail) []string {
	var members []string
	if raw := ch.Fields["members"]; raw != "" {
		_ = json.Unmarshal([]byte(raw), &members)
	}
	return members
}

// nextChannelMembers is members with who added (join) or removed (leave), and
// whether that changes anything.
func nextChannelMembers(members []string, who string, join bool) ([]string, bool) {
	has := slices.Contains(members, who)
	switch {
	case join && !has:
		return append(slices.Clone(members), who), true
	case !join && has:
		return slices.DeleteFunc(slices.Clone(members), func(m string) bool { return m == who }), true
	}
	return members, false
}

func runChannelMembership(cmd *cobra.Command, ref string, join bool) error {
	land, err := channelLand()
	if err != nil {
		return err
	}
	who := channelMemberFlag
	if who == "" {
		if who = channelSelf(); who == "" {
			return fmt.Errorf("cannot tell who you are: run gb login, or pass --member oidc:<email> or agent:<name>")
		}
	}
	ch, err := resolveChannel(cmd.Context(), land, ref)
	if err != nil {
		return err
	}
	if kind := ch.Fields["channel_kind"]; kind != "user" {
		return fmt.Errorf("%s is a %s channel: only a user channel has members to join or leave", ch.Title, kind)
	}
	next, changed := nextChannelMembers(channelMemberList(ch), who, join)
	verb, prep := "joined", "to"
	if !join {
		verb, prep = "left", "from"
	}
	if !changed {
		fmt.Printf("%s has already %s %s (%s)\n", who, verb, ch.Title, ch.ID)
		return nil
	}
	if err := daemon.UpdateBeadFieldsTyped(cmd.Context(), ch.ID, map[string]any{"members": next}); err != nil {
		return fmt.Errorf("changing members of %s: %w", ch.ID, err)
	}
	if who == channelMemberFlag {
		verb = map[bool]string{true: "added", false: "removed"}[join]
		fmt.Printf("%s %s %s %s (%s)\n", verb, who, prep, ch.Title, ch.ID)
		return nil
	}
	fmt.Printf("%s %s %s (%s) in land %s\n", who, verb, ch.Title, ch.ID, land)
	return nil
}

func runChannelMembers(cmd *cobra.Command, args []string) error {
	land, err := channelLand()
	if err != nil {
		return err
	}
	ch, err := resolveChannel(cmd.Context(), land, args[0])
	if err != nil {
		return err
	}
	members := channelMemberList(ch)
	if jsonOutput {
		printJSON(members)
		return nil
	}
	if !(conversation.Channel{Kind: ch.Fields["channel_kind"]}).MembersOnly() {
		fmt.Printf("%s (%s) is open to everyone in land %s\n", ch.Title, ch.ID, land)
		return nil
	}
	if len(members) == 0 {
		fmt.Printf("%s (%s) has no members yet\n", ch.Title, ch.ID)
		return nil
	}
	for _, m := range members {
		fmt.Println(m)
	}
	return nil
}

func init() {
	for _, c := range []*cobra.Command{channelJoinCmd, channelLeaveCmd, channelMembersCmd} {
		c.Flags().StringVar(&channelProject, "project", "", "land (project) of the channel (default: KD_PROJECT / BOAT_PROJECT)")
	}
	for _, c := range []*cobra.Command{channelJoinCmd, channelLeaveCmd} {
		c.Flags().StringVar(&channelMemberFlag, "member", "", "someone else, oidc:<email> or agent:<name> (a land owner's move)")
	}
	channelCmd.AddCommand(channelJoinCmd, channelLeaveCmd, channelMembersCmd)
}
