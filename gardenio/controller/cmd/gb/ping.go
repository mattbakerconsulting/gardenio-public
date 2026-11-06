package main

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"
)

// ping.go — `gb ping`, a minimal daemon-liveness probe (audit rec 4,
// task ka-0000000432 / parent ka-0000007604; evidence bundle ka-0000001955).
//
// WHY IT EXISTS. The gardener-release-owner Step-7 prod-UAT smoke (doc
// ka-0000000327) used `gb news` purely as a daemon round-trip probe — six
// release-owner sessions parsed the FULL news digest solely to confirm "the
// controller + bridge are live". That is both wasteful (a large relevance-
// scored digest for a yes/no liveness answer) and UNSAFE as a probe: `gb
// news` exits 0 on an EMPTY result, so a caller that greps its output for a
// nonempty match (session d31q) recorded a FALSE-POSITIVE PASS when jq
// returned nothing. A liveness probe must fail loudly — nonzero exit — when
// the daemon does not answer, and never depend on the daemon returning any
// particular rows.
//
// `gb ping` is that probe: it issues GET /v1/health (the same DB-ping
// endpoint the kubelet startupProbe uses — daemon.Health), and its process
// exit code IS the whole signal:
//
//   - daemon answers  → exit 0, prints "ok"
//   - daemon errors / times out → exit 1, prints the cause on stderr
//
// It is the cheap, unambiguous replacement for the `gb news OK` idiom in
// Step-7 smoke. It does NOT depend on any bead existing, so it cannot record
// the empty-output false positive.
//
// Through a land door (--http-url ending /land/<name>, as a demo guest or a
// tenant uses, gd-c6LzZkk0qy) /v1/health is not reachable: the door lists no
// credential-free route (kbeads land_door_routes.txt). There the probe is
// GET /v1/me, the door's own admission check, so ok there also means the
// caller's sign-in was let in.

var pingTimeout time.Duration

var pingCmd = &cobra.Command{
	Use:   "ping",
	Short: "Health-probe the beads daemon (round-trip); nonzero exit on failure",
	Long: `Health-probe the beads daemon with a single GET /v1/health round-trip.
Through a land door (--http-url ending /land/<name>) it is GET /v1/me instead,
which the door admits only for a signed-in member or guest of that land.

This is a liveness probe, not a data query: it confirms the controller + beads
daemon are answering, and NOTHING about the returned rows. The process exit
code is the whole signal — 0 when the daemon answers, 1 when it errors or times
out — so a caller can branch on ` + "`$?`" + ` alone:

  gb ping && echo "daemon live" || echo "daemon DOWN"

Prefer this over ` + "`gb news`" + ` as a Step-7 UAT liveness check. ` + "`gb news`" + ` exits
0 on an EMPTY result, so grepping its output for a nonempty match can record a
false-positive PASS when the digest is legitimately empty; ` + "`gb ping`" + ` has no
such failure mode because it does not depend on any bead existing.

With --json, emits {"ok":true} on success or {"ok":false,"error":"..."} on
failure (still exits nonzero on failure).`,
	GroupID: "orchestration",
	Args:    cobra.NoArgs,
	RunE:    runPing,
}

func init() {
	pingCmd.Flags().DurationVar(&pingTimeout, "timeout", 10*time.Second,
		"max wall-clock to wait for the daemon health round-trip")
}

func runPing(cmd *cobra.Command, _ []string) error {
	if daemon == nil {
		return fmt.Errorf("beads daemon client not initialized")
	}

	ctx := cmd.Context()
	if pingTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, pingTimeout)
		defer cancel()
	}

	var err error
	if landDoor() != "" {
		_, err = daemon.Me(ctx)
	} else {
		err = daemon.Health(ctx)
	}

	if jsonOutput {
		out := map[string]any{"ok": err == nil}
		if err != nil {
			out["error"] = err.Error()
		}
		printJSON(out)
		if err != nil {
			// The JSON verdict is already on stdout; carry the failure in the
			// exit code without a duplicate stderr "Error:" line.
			cmd.SilenceErrors = true
			return err
		}
		return nil
	}

	if err != nil {
		return err
	}
	cmd.Println("ok")
	return nil
}
