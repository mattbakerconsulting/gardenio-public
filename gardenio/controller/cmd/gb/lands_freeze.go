package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/lands/freeze"
	"gardenio/controller/internal/pausegate"

	"github.com/spf13/cobra"
)

// gb lands freeze|thaw and pause|resume are the Lands kill switches (Track B
// spec §5.5, step 10.5).
//
//   - freeze|thaw <land> flips one tenant land's land_frozen
//     (internal/lands/freeze, the same core as the Freeze button in /garden):
//     spawns refused, running agents parked, the steward's gardener paused and
//     the members' Commons posts refused, until it is thawed.
//   - pause|resume opens and closes the lands-pause gate: no tenant land
//     starts an agent while it is open. It fails closed, so a gate the
//     controller cannot read refuses tenant spawns too. Running agents carry
//     on; freeze a land to park them.

var landsFreezeCmd = &cobra.Command{
	Use:   "freeze <land>",
	Short: "Freeze a tenant land: refuse spawns and Commons posts, park its agents",
	Long: `Sets land_frozen on the tenant land's project bead (admin only). While it is set:

  - new spawns into the land are refused (spawn_wall=land_frozen), and the
    steward's gardener does not fire;
  - the purse watcher nudges every running agent to push, then suspends it
    after the grace (park_reason=land_frozen) and blocks the land's LiteLLM
    key until the thaw;
  - the land's own-machine agents are revoked, and none is minted until the
    thaw;
  - the land's members cannot post in the Commons, and see a banner.

The reason is the operator's record (on the project bead and its comment); the
tenant is not shown it. gb lands thaw reverses the freeze; agents it suspended
stay parked until someone resumes them.`,
	Example: `  gb lands freeze tomato-patch --reason "AUP: crypto mining, report gd-abc"`,
	Args:    cobra.ExactArgs(1),
	RunE:    runLandsFreeze,
}

var landsThawCmd = &cobra.Command{
	Use:   "thaw <land>",
	Short: "Thaw a frozen tenant land: spawns and Commons posts open again",
	Args:  cobra.ExactArgs(1),
	RunE:  runLandsThaw,
}

var landsPauseCmd = &cobra.Command{
	Use:   "pause",
	Short: "Stop every tenant land from starting agents (opens the lands-pause gate)",
	Long: `Opens the lands-pause gate (a type:gate bead titled lands-pause). While it is
open, no tenant land starts an agent or fires a gardener (spawn_wall=lands_paused);
operator lands are untouched, and running agents carry on. Unlike gardeners-pause
it fails closed: if the controller cannot read the gate, tenant spawns are
refused. gb lands resume closes it.`,
	Example: `  gb lands pause --reason "incident gd-abc: tenant pods escaping egress"`,
	Args:    cobra.NoArgs,
	RunE:    runLandsPause,
}

var landsResumeCmd = &cobra.Command{
	Use:   "resume",
	Short: "Let tenant lands start agents again (closes the lands-pause gate)",
	Args:  cobra.NoArgs,
	RunE:  runLandsResume,
}

func printFreezeState(w io.Writer, s freeze.State, verb string) {
	switch {
	case !s.Changed && s.Frozen:
		fmt.Fprintf(w, "%s is already frozen (by %s at %s: %s)\n", s.Land, orNone(s.By), orNone(s.At), orNone(s.Reason))
	case !s.Changed:
		fmt.Fprintf(w, "%s is not frozen\n", s.Land)
	case s.Frozen:
		fmt.Fprintf(w, "%s frozen (%s): spawns refused, running agents parked after the grace, Commons posts closed to members\n", s.Land, s.ProjectID)
	default:
		fmt.Fprintf(w, "%s thawed (%s): spawns and Commons posts open again; agents the freeze suspended stay parked until resumed\n", s.Land, s.ProjectID)
	}
	if s.Warning != "" {
		fmt.Fprintf(w, "warning: %s %s, but %s\n", s.Land, verb, s.Warning)
	}
}

func runLandsFreeze(cmd *cobra.Command, args []string) error {
	reason, _ := cmd.Flags().GetString("reason")
	s, err := freeze.Freeze(cmd.Context(), daemon, args[0], reason, landsOperator(), time.Now())
	if err != nil {
		return err
	}
	if jsonOutput {
		return json.NewEncoder(os.Stdout).Encode(s)
	}
	printFreezeState(os.Stdout, s, "is frozen")
	return nil
}

func runLandsThaw(cmd *cobra.Command, args []string) error {
	s, err := freeze.Thaw(cmd.Context(), daemon, args[0], landsOperator())
	if err != nil {
		return err
	}
	if jsonOutput {
		return json.NewEncoder(os.Stdout).Encode(s)
	}
	printFreezeState(os.Stdout, s, "is thawed")
	return nil
}

// landsGateStore is the slice of the daemon client pause and resume use.
type landsGateStore interface {
	pausegate.Client
	CloseBead(ctx context.Context, beadID string, fields map[string]string) error
	AddComment(ctx context.Context, beadID, author, text string) error
}

// landsGate is the outcome of pause or resume, gb's --json output.
type landsGate struct {
	Gate    string `json:"gate"`
	Paused  bool   `json:"paused"`
	Changed bool   `json:"changed"`
	Warning string `json:"warning,omitempty"`
}

// pauseLands opens the lands-pause gate, or leaves an open one alone.
func pauseLands(ctx context.Context, st landsGateStore, reason, by string) (landsGate, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return landsGate{}, fmt.Errorf("a pause needs a reason (--reason): it is the operator's record of why")
	}
	g, err := pausegate.Find(ctx, st, pausegate.LandsPauseTitle)
	if err != nil {
		return landsGate{}, fmt.Errorf("reading the %s gate: %w", pausegate.LandsPauseTitle, err)
	}
	if g != nil {
		return landsGate{Gate: g.ID, Paused: true}, nil
	}
	id, err := pausegate.Open(ctx, st, pausegate.LandsPauseTitle, by)
	if err != nil {
		return landsGate{}, fmt.Errorf("opening the %s gate: %w", pausegate.LandsPauseTitle, err)
	}
	out := landsGate{Gate: id, Paused: true, Changed: true}
	if err := st.AddComment(ctx, id, landsGateAuthor(by), fmt.Sprintf("Every tenant land paused by %s: %s. No tenant agent starts until gb lands resume closes this gate.", orNone(by), reason)); err != nil {
		out.Warning = "commenting the reason on the gate: " + err.Error()
	}
	return out, nil
}

// resumeLands closes every open lands-pause gate (two can exist when two
// operators paused at once).
func resumeLands(ctx context.Context, st landsGateStore, by string) (landsGate, error) {
	out := landsGate{}
	for {
		g, err := pausegate.Find(ctx, st, pausegate.LandsPauseTitle)
		if err != nil {
			return out, fmt.Errorf("reading the %s gate: %w", pausegate.LandsPauseTitle, err)
		}
		if g == nil {
			return out, nil
		}
		if out.Gate == g.ID {
			return out, fmt.Errorf("the %s gate %s is still open after closing it", pausegate.LandsPauseTitle, g.ID)
		}
		_ = st.AddComment(ctx, g.ID, landsGateAuthor(by), fmt.Sprintf("Tenant lands resumed by %s.", orNone(by)))
		if err := st.CloseBead(ctx, g.ID, nil); err != nil {
			return out, fmt.Errorf("closing the %s gate %s: %w", pausegate.LandsPauseTitle, g.ID, err)
		}
		out.Gate, out.Changed = g.ID, true
	}
}

// landsGateAuthor signs a gate comment: the operator, or gb when none is known.
func landsGateAuthor(by string) string {
	if by == "" {
		return "gb"
	}
	return by
}

func runLandsPause(cmd *cobra.Command, _ []string) error {
	reason, _ := cmd.Flags().GetString("reason")
	g, err := pauseLands(cmd.Context(), daemon, reason, landsOperator())
	if err != nil {
		return err
	}
	if jsonOutput {
		return json.NewEncoder(os.Stdout).Encode(g)
	}
	if !g.Changed {
		fmt.Printf("tenant lands are already paused (%s gate %s)\n", pausegate.LandsPauseTitle, g.Gate)
		return nil
	}
	fmt.Printf("tenant lands paused (%s gate %s): no tenant agent starts until gb lands resume\n", pausegate.LandsPauseTitle, g.Gate)
	if g.Warning != "" {
		fmt.Printf("warning: paused, but %s\n", g.Warning)
	}
	return nil
}

func runLandsResume(cmd *cobra.Command, _ []string) error {
	g, err := resumeLands(cmd.Context(), daemon, landsOperator())
	if err != nil {
		return err
	}
	if jsonOutput {
		return json.NewEncoder(os.Stdout).Encode(g)
	}
	if !g.Changed {
		fmt.Println("tenant lands are not paused")
		return nil
	}
	fmt.Printf("tenant lands resumed (%s gate %s closed)\n", pausegate.LandsPauseTitle, g.Gate)
	return nil
}

// The daemon client is what both pairs write through.
var (
	_ freeze.Daemon  = (*beadsapi.Client)(nil)
	_ landsGateStore = (*beadsapi.Client)(nil)
)

func init() {
	landsFreezeCmd.Flags().String("reason", "", "why the land is frozen (required; the operator's record, not shown to the tenant)")
	landsPauseCmd.Flags().String("reason", "", "why every tenant land is paused (required)")
	landsCmd.AddCommand(landsFreezeCmd, landsThawCmd, landsPauseCmd, landsResumeCmd)
}
