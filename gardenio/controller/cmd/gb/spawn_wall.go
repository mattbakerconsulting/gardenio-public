package main

import (
	"context"
	"fmt"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/lands/wall"
)

// spawnWallWait is how long gb spawn waits for the controller's verdict on a
// new agent in a tenant land (the Lands spawn wall, step 8.3). The admission
// runs on the spawn event, so a refusal lands within a second or two.
var spawnWallWait = 15 * time.Second

// spawnWallPoll is the interval between reads of the new agent bead.
var spawnWallPoll = time.Second

// spawnWallReader is the slice of the daemon client awaitSpawnWall reads.
type spawnWallReader interface {
	ListProjectBeads(ctx context.Context) (map[string]beadsapi.ProjectInfo, error)
	GetBead(ctx context.Context, beadID string) (*beadsapi.BeadDetail, error)
}

// awaitSpawnWall waits for the controller to decide a new agent in a tenant
// land: refused=true with the refusal when the wall stamped the bead
// (agent_state=budget_exceeded and spawn_wall), false once a pod exists or the
// wait runs out. An operator land, or a project that cannot be read, returns
// at once: the wall never refuses those, so there is nothing to wait for.
func awaitSpawnWall(ctx context.Context, d spawnWallReader, project, beadID string, wait, every time.Duration) (wall.Refusal, bool) {
	if d == nil || beadID == "" {
		return wall.Refusal{}, false
	}
	projects, err := d.ListProjectBeads(ctx)
	if err != nil {
		return wall.Refusal{}, false
	}
	if p, ok := projects[project]; !ok || p.IsOperatorLand() {
		return wall.Refusal{}, false
	}
	deadline := time.Now().Add(wait)
	for {
		if b, err := d.GetBead(ctx, beadID); err == nil && b != nil {
			f := b.Fields
			if f["agent_state"] == wall.AgentState && f[wall.FieldCode] != "" {
				return wall.Refusal{Code: f[wall.FieldCode], Reason: f[wall.FieldReason]}, true
			}
			if f["pod_name"] != "" || f["pod_phase"] != "" {
				return wall.Refusal{}, false
			}
		}
		if !time.Now().Add(every).Before(deadline) {
			return wall.Refusal{}, false
		}
		select {
		case <-ctx.Done():
			return wall.Refusal{}, false
		case <-time.After(every):
		}
	}
}

// reportSpawnWallRefusal prints a spawn the Lands spawn wall refused and
// returns an error, so the command exits non-zero. The agent bead exists: it
// stays open at agent_state=budget_exceeded and the controller starts it once
// the land is clear.
func reportSpawnWallRefusal(agentName, beadID, project, role, taskID string, r wall.Refusal) error {
	if jsonOutput {
		result := map[string]string{
			"id": beadID, "name": agentName, "project": project, "role": role,
			"status": "refused", "reason": r.Code, "detail": r.Reason,
		}
		if taskID != "" {
			result["task"] = taskID
		}
		printJSON(result)
	} else {
		fmt.Printf("REFUSED agent=%s bead=%s project=%s reason=%s\n", agentName, beadID, project, r.Code)
		fmt.Printf("  The Lands spawn wall refused this spawn: %s\n", r.Reason)
		fmt.Printf("  The agent bead stays open (agent_state=%s) and starts once the land is clear; close it with 'kd close %s' to give up.\n", wall.AgentState, beadID)
	}
	return fmt.Errorf("spawn of %s refused by the Lands spawn wall (%s)", agentName, r.Code)
}

// spawnWallDaemon is the global daemon client as a spawnWallReader, nil when
// there is none (an interface holding a nil *Client is not nil).
func spawnWallDaemon() spawnWallReader {
	if daemon == nil {
		return nil
	}
	return daemon
}
