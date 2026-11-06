package scheduler

import (
	"encoding/json"
	"strings"
	"time"
)

// MaxRunLogEntries bounds the rolling run_log on each schedule bead. Older
// entries are dropped on append; this keeps the field's serialized size
// well under the bead-field budget even for schedules that have run for
// months.
const MaxRunLogEntries = 50

// RunLogEntry is one closed-run summary written to the schedule bead's
// `run_log` field by trackLastAgentStatus. It supersedes the per-run
// "Run completed → ka-X" history comments the scheduler used to write —
// those comments accumulated unbounded and blew out agent context after
// hundreds of cycles.
//
// Only terminal-state fields are recorded; the spawn-time bookkeeping
// stays in the load-bearing `last_run` / `last_agent_id` pair.
type RunLogEntry struct {
	Agent         string    `json:"agent,omitempty"`
	StartedAt     time.Time `json:"started_at,omitzero"`
	EndedAt       time.Time `json:"ended_at,omitzero"`
	DurationS     int       `json:"duration_s,omitempty"`
	Outcome       string    `json:"outcome,omitempty"` // agent_state: done | failed | ...
	CloseReason   string    `json:"close_reason,omitempty"`
	WrapupSummary string    `json:"wrapup_summary,omitempty"`
}

// RunLog is the rolling list of recent runs, newest-last. Stored as a
// flat JSON array in the schedule bead's `run_log` field.
type RunLog []RunLogEntry

// ParseRunLog parses an opaque `run_log` JSON field. Empty input returns
// an empty log with no error.
func ParseRunLog(raw string) (RunLog, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var rl RunLog
	if err := json.Unmarshal([]byte(raw), &rl); err != nil {
		return nil, err
	}
	return rl, nil
}

// Marshal serializes the log for storage on the schedule bead. An empty
// log marshals to "[]" so the field can be cleared / truncated by
// writing back the result of Marshal on a freshly-constructed RunLog.
func (rl RunLog) Marshal() (string, error) {
	if rl == nil {
		rl = RunLog{}
	}
	b, err := json.Marshal(rl)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// Append adds one entry, dropping the oldest entries once the slice
// exceeds MaxRunLogEntries.
func (rl *RunLog) Append(entry RunLogEntry) {
	*rl = append(*rl, entry)
	if len(*rl) > MaxRunLogEntries {
		*rl = (*rl)[len(*rl)-MaxRunLogEntries:]
	}
}
