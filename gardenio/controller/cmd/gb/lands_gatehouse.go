package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"

	"github.com/spf13/cobra"
)

// gb lands gatehouse demo|apply are the gatehouse steward's tools (task
// gd-K62coPtKkX, decision D4 apply-and-demo). Each files one
// type=gatehouse_request in the gatehouse naming the message the person wrote,
// then waits for the viewer's gatehouse reactor to answer it and prints the
// answer for the steward to pass on. The steward never says who the person is:
// the reactor takes them from the GitHub account kbeads stamped on the message.
// kbeads admits the create only from the gatehouse's steward, and only while
// the gatehouse carries land_gatehouse_tools=true.

const (
	gatehouseRequestType = "gatehouse_request"
	gatehouseWaitDefault = 60 * time.Second
	gatehousePoll        = 2 * time.Second
)

var landsGatehouseCmd = &cobra.Command{
	Use:   "gatehouse",
	Short: "The gatehouse steward's tools: a demo pass, an application draft",
}

var landsGatehouseDemoCmd = &cobra.Command{
	Use:   "demo <gatehouse> --message <id>",
	Short: "Ask for a demo-land guest pass for the person who wrote a message",
	Long: `Files a demo request for the person who wrote --message, in the gatehouse land,
and waits for the answer. The pass is for whoever the daemon verified wrote the
message (their GitHub sign-in), never for a name you give. The viewer checks
their terms, one pass per GitHub account and the cap on open passes, and keeps a
draft. Nothing is filed until the person opens /lands/demo, sees their message
quoted and presses Get a guest pass. One request per message; the gatehouse
files a bounded number an hour.`,
	Example: `  gb lands gatehouse demo gatehouse --message gd-m1`,
	Args:    cobra.ExactArgs(1),
	RunE:    runLandsGatehouseDemo,
}

var landsGatehouseApplyCmd = &cobra.Command{
	Use:   "apply <gatehouse> --message <id> --land-name <name> --purpose <text>",
	Short: "Draft a plot application for the person who wrote a message",
	Long: `Files an application request for the person who wrote --message, with the land
name, purpose and credits they told you, and waits for the answer. The viewer
checks them as the /lands/apply form does and keeps a draft. The person then
opens /lands/apply, connects their repository (only they can prove they
control it), sees their message quoted, chooses to use the draft, checks the
form and presses Apply.`,
	Example: `  gb lands gatehouse apply gatehouse --message gd-m1 --land-name tomato-patch \
    --purpose "Keep the docs in step with the code" --credits 25`,
	Args: cobra.ExactArgs(1),
	RunE: runLandsGatehouseApply,
}

// gatehouseAPI is the daemon surface the verbs use.
type gatehouseAPI interface {
	CreateBeadDetail(ctx context.Context, req beadsapi.CreateBeadRequest) (*beadsapi.BeadDetail, error)
	GetBead(ctx context.Context, beadID string) (*beadsapi.BeadDetail, error)
}

func runLandsGatehouseDemo(cmd *cobra.Command, args []string) error {
	msg, _ := cmd.Flags().GetString("message")
	wait, _ := cmd.Flags().GetDuration("wait")
	b, err := fileGatehouseRequest(cmd.Context(), daemon, args[0], "Demo pass for the writer of "+msg,
		map[string]any{"kind": "demo", "message_id": msg}, wait, time.Sleep)
	return printGatehouseAnswer(b, err)
}

func runLandsGatehouseApply(cmd *cobra.Command, args []string) error {
	msg, _ := cmd.Flags().GetString("message")
	name, _ := cmd.Flags().GetString("land-name")
	purpose, _ := cmd.Flags().GetString("purpose")
	wait, _ := cmd.Flags().GetDuration("wait")
	fields := map[string]any{"kind": "apply", "message_id": msg, "land_name": strings.ToLower(strings.TrimSpace(name)), "purpose": strings.TrimSpace(purpose)}
	if cmd.Flags().Changed("credits") {
		credits, _ := cmd.Flags().GetFloat64("credits")
		fields["credits_requested_usd"] = credits
	}
	b, err := fileGatehouseRequest(cmd.Context(), daemon, args[0], "Application draft for the writer of "+msg, fields, wait, time.Sleep)
	return printGatehouseAnswer(b, err)
}

// fileGatehouseRequest files the request and polls it until the reactor
// answers or wait runs out. On a timeout it returns the pending bead with no
// error: the answer still comes, and the steward can read it later.
func fileGatehouseRequest(ctx context.Context, api gatehouseAPI, land, title string, fields map[string]any, wait time.Duration, sleep func(time.Duration)) (*beadsapi.BeadDetail, error) {
	if strings.TrimSpace(land) == "" {
		return nil, errors.New("name the gatehouse land")
	}
	if id, _ := fields["message_id"].(string); strings.TrimSpace(id) == "" {
		return nil, errors.New("--message names the message the person wrote")
	}
	fields["state"] = "pending"
	raw, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	b, err := api.CreateBeadDetail(ctx, beadsapi.CreateBeadRequest{
		Title: title, ShortTitle: "gatehouse " + fields["kind"].(string) + " request", Type: gatehouseRequestType, Kind: "data",
		Labels: []string{"project:" + land}, Fields: raw,
	})
	if err != nil {
		return nil, fmt.Errorf("filing the request: %w", err)
	}
	for waited := time.Duration(0); waited < wait; waited += gatehousePoll {
		if ctx.Err() != nil {
			break
		}
		sleep(gatehousePoll)
		got, err := api.GetBead(ctx, b.ID)
		if err != nil {
			continue
		}
		b = got
		if b.Fields["state"] != "pending" {
			break
		}
	}
	return b, nil
}

func printGatehouseAnswer(b *beadsapi.BeadDetail, err error) error {
	if err != nil {
		return err
	}
	if jsonOutput {
		return json.NewEncoder(os.Stdout).Encode(map[string]string{
			"id": b.ID, "state": b.Fields["state"], "outcome": b.Fields["outcome"], "result_id": b.Fields["result_id"],
		})
	}
	switch state := b.Fields["state"]; state {
	case "pending":
		fmt.Printf("%s: not answered yet; read it later with kd show %s\n", b.ID, b.ID)
	default:
		fmt.Printf("%s: %s\n%s\n", b.ID, state, b.Fields["outcome"])
	}
	return nil
}

func init() {
	for _, c := range []*cobra.Command{landsGatehouseDemoCmd, landsGatehouseApplyCmd} {
		c.Flags().String("message", "", "the message the person wrote (required)")
		c.Flags().Duration("wait", gatehouseWaitDefault, "how long to wait for the answer")
		_ = c.MarkFlagRequired("message")
	}
	landsGatehouseApplyCmd.Flags().String("land-name", "", "the land name they asked for (required)")
	landsGatehouseApplyCmd.Flags().String("purpose", "", "what they want the agents to work on, in their words (required)")
	landsGatehouseApplyCmd.Flags().Float64("credits", 0, "credits they asked for (optional)")
	_ = landsGatehouseApplyCmd.MarkFlagRequired("land-name")
	_ = landsGatehouseApplyCmd.MarkFlagRequired("purpose")
	landsGatehouseCmd.AddCommand(landsGatehouseDemoCmd, landsGatehouseApplyCmd)
	landsCmd.AddCommand(landsGatehouseCmd)
}
