// Orphaned-fire recovery for task-keyed spawn_agent triggers (gd-CZnK7jeKSG).
//
// A spawn_agent fire is two writes: createTaskBead files the per-fire task
// (carrying the source's fire key, e.g. schedule_fire_key), then SpawnAgent
// creates the agent. When the second write never lands (a 403 spawn denial, a
// daemon outage, the engine pod dying between the two), the task bead is left
// behind with no agent. The pre-fire dedup refresh (ka-0000000151) and the
// startup catch-up both read task beads through CatchUpKeys, so that orphan
// task then counts as "already fired" and the window is dropped for good,
// with nothing in the data plane saying so. The herder trigger gd-AHklx9rH70
// lost its 04:41Z window this way (spawn denied) and its 18:41Z window too
// (task gd-tRepeLy9JA created 18:47:17Z, no agent, no audit row).
//
// The repair: for a source that declares which task field carries its fire
// key (fireTaskKeyedSource), a match the seen set suppresses is checked
// against the agent beads before it is given up. If no agent was spawned for
// the key and the key's task bead was never claimed, the window is re-fired,
// reusing that task bead when it is still open so a spawn that keeps failing
// does not file a new task on every cycle (the duplicate ka-0000000151 was
// written to stop). A re-fire waits orphanRefireBackoff after the orphan task
// was created and after each failed re-fire, so a fire still in flight is not
// raced and a spawn that keeps failing retries every few minutes rather than
// every cycle. Every case writes a note into last_outcome.

package bridge

import (
	"context"
	"fmt"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// orphanRefireBackoff is how long the engine waits, after an orphan task bead
// was created or after its last re-fire attempt, before it re-fires the window.
const orphanRefireBackoff = 5 * time.Minute

// fireTaskKeyedSource is an optional interface a TriggerSource implements to
// name the task-bead field its CatchUpKeys reads the fire key from. Only such
// sources get orphaned-fire recovery. A source opts in only when its Evaluate
// already stops producing a match once the fire is recorded (the schedule
// source's last_run / last_match guard), so the recovery lookups run only for
// a window that was deduped without a recorded fire, not on every cycle.
type fireTaskKeyedSource interface {
	FireTaskKeyField() string
}

// orphanRecoveryEligible reports whether trig's suppressed matches may be
// re-checked for orphaned fires: a spawn_agent trigger that files a per-fire
// task, on a source that names its task key field. An AssignBead gardener
// files no per-fire task, so there is nothing to orphan.
func orphanRecoveryEligible(source TriggerSource, trig TriggerDef) (string, bool) {
	if trig.Action != TriggerActionSpawnAgent {
		return "", false
	}
	if trig.Gardener != nil && trig.Gardener.AssignBead != "" {
		return "", false
	}
	ks, ok := source.(fireTaskKeyedSource)
	if !ok || ks.FireTaskKeyField() == "" {
		return "", false
	}
	return ks.FireTaskKeyField(), true
}

// refireOrphanedMatches checks each match the seen set suppressed and returns
// the ones whose fire never spawned an agent and are due a re-fire, with their
// keys removed from the seen set and orphanTaskID set when the task is still
// open. notes holds one line per orphan, for last_outcome. Lookup errors keep
// the match suppressed: a daemon hiccup must not cause a duplicate spawn.
func (e *TriggerEngine) refireOrphanedMatches(ctx context.Context, source TriggerSource, trig TriggerDef, suppressed []TriggerMatch) (refire []TriggerMatch, notes []string) {
	keyField, ok := orphanRecoveryEligible(source, trig)
	if !ok {
		return nil, nil
	}
	now := e.now()
	for _, m := range suppressed {
		if m.Key == "" {
			continue
		}
		agent, err := e.findExistingSpawnForMatch(ctx, trig, m)
		if err != nil {
			e.logger.Warn("triggers: orphan check: agent lookup failed, keeping match deduped",
				"trigger", trig.ID, "match", m.Key, "error", err)
			continue
		}
		if agent != "" {
			continue
		}
		task, err := e.findFireTask(ctx, trig, keyField, m.Key)
		if err != nil {
			e.logger.Warn("triggers: orphan check: task lookup failed, keeping match deduped",
				"trigger", trig.ID, "match", m.Key, "error", err)
			continue
		}
		if task == nil || task.Assignee != "" {
			// No task (the seen key came from this engine's own fire) or someone
			// claimed it: not an orphan of a failed spawn.
			continue
		}

		due := task.CreatedAt.Add(orphanRefireBackoff)
		e.mu.Lock()
		if last, ok := e.orphanRefireAt[trig.ID][m.Key]; ok && last.Add(orphanRefireBackoff).After(due) {
			due = last.Add(orphanRefireBackoff)
		}
		e.mu.Unlock()
		if now.Before(due) {
			notes = append(notes, fmt.Sprintf("%s: task %s has no spawned agent; re-fire at %s",
				m.Key, task.ID, due.UTC().Format("15:04:05Z")))
			e.logger.Warn("triggers: fire left a task with no agent, re-fire waiting on backoff",
				"trigger", trig.ID, "match", m.Key, "task", task.ID, "due", due.UTC().Format(time.RFC3339))
			continue
		}

		e.mu.Lock()
		delete(e.seen[trig.ID], m.Key)
		if at := e.seenAt[trig.ID]; at != nil {
			delete(at, m.Key)
		}
		if e.orphanRefireAt[trig.ID] == nil {
			e.orphanRefireAt[trig.ID] = make(map[string]time.Time)
		}
		e.orphanRefireAt[trig.ID][m.Key] = now
		e.mu.Unlock()

		if task.Status == "open" {
			m.orphanTaskID = task.ID
		}
		notes = append(notes, fmt.Sprintf("re-fired %s: task %s had no spawned agent", m.Key, task.ID))
		e.logger.Warn("triggers: fire left a task with no agent, re-firing the window",
			"trigger", trig.ID, "match", m.Key, "task", task.ID, "task_status", task.Status)
		refire = append(refire, m)
	}
	return refire, notes
}

// findFireTask returns the newest unclaimed task bead this trigger filed for
// key, preferring an open one, or a claimed one when every task for the key
// is claimed. nil when no task carries the key.
func (e *TriggerEngine) findFireTask(ctx context.Context, trig TriggerDef, keyField, key string) (*beadsapi.BeadDetail, error) {
	res, err := e.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Kinds:    []string{"issue"},
		Statuses: []string{"open", "in_progress", "closed"},
		Labels:   []string{"trigger:" + trig.ID},
		Fields:   map[string]string{keyField: key},
		Limit:    10,
	})
	if err != nil {
		return nil, err
	}
	if res == nil {
		return nil, nil
	}
	var best *beadsapi.BeadDetail
	for _, b := range res.Beads {
		if b == nil {
			continue
		}
		if b.Assignee != "" {
			// A claimed task means someone is on the window: report it so the
			// caller keeps the match deduped.
			return b, nil
		}
		switch {
		case best == nil:
			best = b
		case b.Status == "open" && best.Status != "open":
			best = b
		case (b.Status == "open") == (best.Status == "open") && b.CreatedAt.After(best.CreatedAt):
			best = b
		}
	}
	return best, nil
}

// clearOrphanRefire forgets the re-fire backoff for key once a fire for it
// succeeds.
func (e *TriggerEngine) clearOrphanRefire(triggerID, key string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if keys := e.orphanRefireAt[triggerID]; keys != nil {
		delete(keys, key)
		if len(keys) == 0 {
			delete(e.orphanRefireAt, triggerID)
		}
	}
}

// withOrphanNotes appends the orphan notes to a last_outcome string.
func withOrphanNotes(outcome string, notes []string) string {
	if len(notes) == 0 {
		return outcome
	}
	return outcome + "; " + strings.Join(notes, "; ")
}
