package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// `gb trigger reseal <id>...` renews the spawn-lineage seal on triggers and
// schedules (gd-VuYwGGYLLT, fix B). The daemon stamps the seal only when a
// gardenio-admin OIDC session writes the bead, and it lets the trigger-engine
// spawn a privileged role for that trigger without an escalation
// (kbeads/internal/server/spawn_lineage_seal.go). A seal lapses after 30 days
// and is never created for a trigger an agent, a static token or a background
// reactor authored, so something has to renew them: this sends the explicit
// reseal_spawn_lineage control key as the signed-in session. The key is consumed
// by the daemon and never stored, and a non-admin session that sends it gets no
// seal.
var triggerResealCmd = &cobra.Command{
	Use:   "reseal <trigger-id>...",
	Short: "Renew the spawn-lineage seal on triggers/schedules (needs a gardenio-admin login)",
	Long: `Renew the spawn-lineage seal on one or more trigger or schedule beads, so the
trigger-engine can spawn their privileged role without an escalation.

A reseal ADOPTS THE CURRENT CONTENT of each bead as the admin's own: whoever last
edited the role, prompt or schedule, you are now vouching for it. For each trigger
the command prints the full content the seal covers (action, role, project,
prompt, triggers, the configuration half of the gardener blob, ...) and its
digest, then sends that digest with the write. If the trigger changed between the
read and the write, the daemon refuses the reseal; re-run it and review the new
content. Ids are explicit on purpose; there is no "reseal everything".

Run it signed in as a gardenio-admin OIDC user (gb login). Any other credential
gets no seal and the daemon says nothing, so the command reads the bead back and
reports whether a seal is now present.`,
	Args: cobra.MinimumNArgs(1),
	RunE: runTriggerReseal,
}

func runTriggerReseal(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	var failed []string
	for _, id := range args {
		b, err := daemon.GetBead(ctx, id)
		if err != nil {
			return fmt.Errorf("reading %s: %w", id, err)
		}
		if b == nil || (b.Type != "trigger" && b.Type != "schedule") {
			return fmt.Errorf("%s is not a trigger or schedule bead", id)
		}
		if b.SpawnLineageDigest == "" {
			// An old daemon cannot pin the digest; adopting unseen content is the
			// exact hole this command closes, so fail rather than reseal blind.
			return fmt.Errorf("%s: the daemon did not report a spawn-lineage digest (daemon too old?); refusing to reseal content that cannot be pinned", id)
		}
		before := b.Fields["spawn_lineage"]
		if !jsonOutput {
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "sealing %s %q: action=%s role=%s project=%s status=%s enabled=%s\n",
				id, b.Title, b.Fields["action"], b.Fields["role"], b.Fields["project"], b.Status, enabledLabel(b.Fields["enabled"]))
			content, err := json.MarshalIndent(b.SpawnLineageContent, "  ", "  ")
			if err != nil {
				return fmt.Errorf("rendering %s: %w", id, err)
			}
			fmt.Fprintf(out, "  content the seal covers (digest %s):\n  %s\n", b.SpawnLineageDigest, content)
		}
		if err := daemon.UpdateBeadFieldsTyped(ctx, id, map[string]any{
			"reseal_spawn_lineage":   true,
			"reseal_expected_digest": b.SpawnLineageDigest,
		}); err != nil {
			return fmt.Errorf("resealing %s: %w", id, err)
		}
		after, err := daemon.GetBead(ctx, id)
		if err != nil {
			return fmt.Errorf("reading %s back: %w", id, err)
		}
		if after == nil || strings.TrimSpace(after.Fields["spawn_lineage"]) == "" || after.Fields["spawn_lineage"] == before {
			failed = append(failed, id)
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s carries no fresh seal after the write; the session is probably not a gardenio-admin OIDC login\n", id)
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("not sealed: %s", strings.Join(failed, ", "))
	}
	if !jsonOutput {
		fmt.Fprintf(cmd.OutOrStdout(), "sealed %d bead(s)\n", len(args))
	}
	return nil
}

// enabledLabel renders a trigger's enabled field, which is absent (true) on
// beads that never set it.
func enabledLabel(v string) string {
	if v == "false" {
		return "false"
	}
	return "true"
}

func init() {
	triggerCmd.AddCommand(triggerResealCmd)
}
