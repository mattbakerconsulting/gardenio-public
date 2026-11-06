// init_agent_mail.go — the agent pod's mail subscriber (spec §3.1).
//
// Agent pods run `gb init` (images/agent/entrypoint.sh), so its restart loop
// is where a pod agent subscribes to its own mail: bridge nudges, decision and
// escalation results, agent-to-agent interrupt mail, control:kill and Slack
// permission verdicts (control:permission, mail_permission.go) all arrive as
// mail (bridge.NudgeAgent), and nothing dials coop from outside.
// `gb agent start --k8s` uses the same helper.

package main

import (
	"context"
	"log/slog"
	"os"
	"sync/atomic"
	"time"

	"gardenio/controller/internal/mailsub"
)

// podMail runs an agent's mail subscriber for the life of a restart loop,
// across coop restarts, so the delivered-mail dedup and the closed-kill
// baseline survive a restart. Mail is delivered to whichever coop is up on
// the loopback port; mail that arrives while none is stays open and reaches
// the next session through `gb hook check-mail`.
//
// A control:kill marks the loop killed before coop is asked to shut down,
// then cancels sessionCtx, which SIGTERMs coop if the shutdown did not take.
// The restart loop checks killRequested after every coop exit and closes the
// agent bead instead of restarting.
type podMail struct {
	killed atomic.Bool
	// sessionCtx is the context coop sessions run under; a control:kill
	// cancels it.
	sessionCtx context.Context
	cancel     context.CancelFunc
}

// startPodMail subscribes as agentBead/agentName and delivers to coop. With
// no daemon or agent bead it starts nothing, and sessionCtx is just ctx.
func startPodMail(ctx context.Context, d mailsub.Daemon, coop mailsub.Coop, agentBead, agentName string, logf func(string, ...any)) *podMail {
	p := &podMail{}
	p.sessionCtx, p.cancel = context.WithCancel(ctx)
	if d == nil || agentBead == "" {
		logf("mail subscriber off: no daemon or agent bead")
		return p
	}
	sub := mailsub.New(mailsub.Config{
		Daemon:    d,
		Coop:      killMarkingCoop{Coop: coop, killed: &p.killed},
		AgentBead: agentBead,
		AgentName: agentName,
		Logger:    slog.Default(),
		OnControl: newPermissionControl(d, logf).OnControl,
		Exit: func() {
			p.killed.Store(true)
			p.cancel()
		},
	})
	go func() {
		logf("mail subscriber started (agent bead %s)", agentBead)
		if err := sub.Run(p.sessionCtx); err != nil && ctx.Err() == nil && !p.killed.Load() {
			logf("mail subscriber stopped: %v", err)
		}
	}()
	return p
}

func (p *podMail) killRequested() bool { return p.killed.Load() }

// stop ends the subscriber when the restart loop returns.
func (p *podMail) stop() { p.cancel() }

// killMarkingCoop marks the kill before forwarding mailsub's Shutdown, the
// first thing it does on control:kill. Exit runs only after the shutdown
// returns, by which time coop may have exited and the restart loop moved on;
// marking here means the loop always sees the kill.
type killMarkingCoop struct {
	mailsub.Coop
	killed *atomic.Bool
}

func (c killMarkingCoop) Shutdown(ctx context.Context) error {
	c.killed.Store(true)
	return c.Coop.Shutdown(ctx)
}

// initMailDaemon is the daemon client as a mailsub.Daemon, nil when there is
// none (a nil *beadsapi.Client would be a non-nil interface).
func initMailDaemon() mailsub.Daemon {
	if daemon == nil {
		return nil
	}
	return daemon
}

// closeAgentBeadOnMailKill closes this agent's bead after a control:kill mail
// stopped coop, so the reconciler does not respawn it.
func closeAgentBeadOnMailKill(logf func(string, ...any)) {
	agentBeadID := envOr("KD_AGENT_ID", os.Getenv("BOAT_AGENT_BEAD_ID"))
	if agentBeadID == "" || daemon == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := closeAgentBeadWithFallback(ctx, agentBeadID, "control_kill", "control_kill",
		map[string]string{"agent_state": "done", "close_reason": "control_kill"}); err != nil {
		logf("warning: close agent bead: %v", err)
	}
}
