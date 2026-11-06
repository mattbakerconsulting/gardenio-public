package main

// agent_stop_sentinel.go — keep a finished agent stopped when it cannot see its
// own bead (gd-aoSsfpwPGG).
//
// The restart loops decide "stop or respawn" by reading stop_requested off the
// agent bead. An agent whose bead the daemon refuses to show it (a label-less
// agent bead outside its project scope answers 403) could neither set the flag
// in `gb stop` nor read it back, so the loop logged "assuming not stopped" and
// respawned coop: the finished agent re-ran its prompt every ~10 minutes and
// re-posted the same comments. Two fixes live here:
//
//   - checkStopRequested tells an authz denial (HTTP 401/403) apart from a
//     transport error, so the loop can PARK instead of respawning; and
//   - a local stop sentinel, written by `gb stop` when the stop_requested write
//     fails and read by the loops as equal to stop_requested, since `gb stop`
//     and the loop share the pod filesystem.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// stopSentinelPath is the local stop sentinel. It lives in /tmp, the pod-scoped
// EmptyDir (podmanager.MountTmp), not on the workspace PVC under $HOME: the PVC
// outlives the pod and the pod name is stable per agent name, so a sentinel
// there would park the next, legitimately respawned pod. /tmp survives a
// container restart (shouldExitBeforeStart still sees it) and dies with the pod.
// A var so tests can point it at a temp dir.
var stopSentinelPath = "/tmp/gb-stop-requested"

// writeStopSentinel records that agentBeadID asked to stop. The file holds the
// bead ID so a sentinel can never stop a different agent.
func writeStopSentinel(agentBeadID string) error {
	return os.WriteFile(stopSentinelPath, []byte(agentBeadID+"\n"), 0o644)
}

// stopSentinelSet reports whether the local sentinel records a stop for
// agentBeadID.
func stopSentinelSet(agentBeadID string) bool {
	if agentBeadID == "" {
		return false
	}
	data, err := os.ReadFile(stopSentinelPath)
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(data)) == agentBeadID
}

// isAuthzDenial reports whether err is the daemon refusing this caller (HTTP 401
// or 403), as opposed to a transport failure or a server error. A denial is
// deterministic: retrying it cannot succeed.
func isAuthzDenial(err error) bool {
	var apiErr *beadsapi.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	return apiErr.StatusCode == http.StatusUnauthorized || apiErr.StatusCode == http.StatusForbidden
}

// stopState is the answer to "did this agent ask to stop?".
type stopState int

const (
	stopNotRequested stopState = iota
	stopRequested
	// stopUnknownDenied: the daemon denied the read of the agent's own bead, and
	// no local sentinel says stop. The loop cannot tell a finished agent from a
	// crashed one, so it must not respawn coop.
	stopUnknownDenied
)

// stopCheckRetryDelay is the pause between isStopRequested retries on a
// transport error. A var so tests do not sleep.
var stopCheckRetryDelay = 2 * time.Second

// checkStopRequested reads the stop request for agentBeadID. The local sentinel
// wins without a daemon read. A denial returns at once with the error; any
// other error is retried up to 3 times and then reads as not requested, as
// before.
func checkStopRequested(ctx context.Context, agentBeadID string) (stopState, error) {
	if agentBeadID == "" {
		return stopNotRequested, nil
	}
	if stopSentinelSet(agentBeadID) {
		return stopRequested, nil
	}
	if daemon == nil {
		return stopNotRequested, nil
	}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		bead, err := daemon.GetBead(checkCtx, agentBeadID)
		cancel()
		if err == nil {
			if bead.Fields["stop_requested"] == "true" {
				return stopRequested, nil
			}
			return stopNotRequested, nil
		}
		lastErr = err
		if isAuthzDenial(err) {
			return stopUnknownDenied, err
		}
		fmt.Printf("[gb agent start] isStopRequested: attempt %d failed: %v\n", attempt+1, err)
		if ctx.Err() != nil {
			return stopNotRequested, err
		}
		time.Sleep(stopCheckRetryDelay)
	}
	fmt.Printf("[gb agent start] isStopRequested: all retries failed, assuming not stopped\n")
	return stopNotRequested, lastErr
}

// parkContainer is how a parked restart loop waits: until the pod is terminated.
// Sleeping rather than exiting keeps RestartPolicy=Always from turning the park
// into a container crash loop. A var so tests can observe the park without
// blocking.
var parkContainer = func(ctx context.Context) { <-ctx.Done() }

// parkLoop logs why a restart loop is parked, through that loop's logf, and
// blocks until ctx is done.
func parkLoop(ctx context.Context, logf func(string, ...any), reason string) {
	logf("PARKED: %s — not respawning coop; the container sleeps until the pod is deleted", reason)
	parkContainer(ctx)
}

// parkInitLoop parks the gb init loop.
func parkInitLoop(ctx context.Context, reason string) {
	parkLoop(ctx, logInit, reason)
}

// logAgentStart logs with the gb agent start loop's prefix.
func logAgentStart(format string, args ...any) {
	fmt.Printf("[gb agent start] "+format+"\n", args...)
}

// deniedOwnBeadReason is the park reason when the daemon denies the read of the
// agent's own bead.
func deniedOwnBeadReason(agentBeadID string, err error) string {
	return fmt.Sprintf("the daemon denied reading this agent's own bead %s (%v), so a gb stop cannot be seen; "+
		"fix the bead's access (for example its project:/role: labels) and delete the pod", agentBeadID, err)
}

// handleStopAfterExit is the init loop's stop check after coop exits. It
// returns true when the loop must not respawn coop: the agent asked to stop
// (bead closed, or parked when the close is denied), or the daemon denied the
// read of the agent's own bead (parked). It returns false to respawn.
func handleStopAfterExit(ctx context.Context, agentBeadID string) bool {
	if agentBeadID == "" {
		return false
	}
	state, err := checkStopRequested(ctx, agentBeadID)
	switch state {
	case stopRequested:
		logInit("stop requested — closing bead and exiting")
		closeOrPark(ctx, agentBeadID)
		return true
	case stopUnknownDenied:
		parkInitLoop(ctx, deniedOwnBeadReason(agentBeadID, err))
		return true
	}
	return false
}

// handleAgentStartStop is handleStopAfterExit for the gb agent start loop
// (gd-WKjtvzPl6g). It closes the agent bead through
// closeAgentBeadWithFallback, so a stop without a wrap-up still gets a
// synthesized headline, and parks when the close or the stop read is denied.
// It returns true when the loop must not respawn coop.
func handleAgentStartStop(ctx context.Context, agentBeadID string) bool {
	if agentBeadID == "" {
		return false
	}
	state, err := checkStopRequested(ctx, agentBeadID)
	switch state {
	case stopRequested:
		logAgentStart("stop requested — closing bead and exiting cleanly")
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 10*time.Second)
		// Route through the shared fallback path so a stop-without-wrap-up
		// still gets a synthesized tl;dr headline (Fix A, ka-0000007639). A
		// real wrap-up already on the bead always wins.
		err := closeAgentBeadWithFallback(closeCtx, agentBeadID, "stop_requested", "stop_requested",
			map[string]string{"agent_state": "done", "close_reason": "stop_requested"})
		closeCancel()
		if err != nil {
			logAgentStart("warning: close agent bead: %v", err)
			// A denied close leaves the bead open; exiting would only bring
			// the container back, as in closeOrPark.
			if isAuthzDenial(err) {
				parkLoop(ctx, logAgentStart, fmt.Sprintf("stop requested but the daemon denied closing agent bead %s (%v)", agentBeadID, err))
			}
		}
		return true
	case stopUnknownDenied:
		parkLoop(ctx, logAgentStart, deniedOwnBeadReason(agentBeadID, err))
		return true
	}
	return false
}

// closeOrPark closes the agent bead after a stop request. When the daemon
// denies the close the bead stays open, and exiting would only bring the
// container back (RestartPolicy=Always), so it parks instead. It reports
// whether it parked.
func closeOrPark(ctx context.Context, agentBeadID string) bool {
	if daemon == nil {
		return false
	}
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 10*time.Second)
	err := daemon.CloseBead(closeCtx, agentBeadID, map[string]string{"agent_state": "done"})
	closeCancel()
	if err == nil {
		return false
	}
	logInit("warning: close agent bead: %v", err)
	if !isAuthzDenial(err) {
		return false
	}
	parkInitLoop(ctx, fmt.Sprintf("stop requested but the daemon denied closing agent bead %s (%v)", agentBeadID, err))
	return true
}
