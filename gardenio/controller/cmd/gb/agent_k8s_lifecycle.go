package main

// agent_k8s_lifecycle.go — session lifecycle utilities.
//
// Handles: stale session log detection, resume candidate validation,
// and coop HTTP helpers for the restart loop.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// cleanStalePipes removes leftover hook.pipe FIFO files from the coop state
// directory before each session start.
func cleanStalePipes(coopStateDir string) {
	sessionsDir := filepath.Join(coopStateDir, "sessions")
	if _, err := os.Stat(sessionsDir); err != nil {
		return
	}
	entries, err := filepath.Glob(filepath.Join(sessionsDir, "*", "hook.pipe"))
	if err != nil {
		return
	}
	for _, p := range entries {
		os.Remove(p)
	}
}

// cleanStaleSessions removes sessions/<pid>.json files whose PID doesn't
// correspond to a running process. After a pod restart every old PID is
// invalid, so these files are always safe to remove. Stale PID files in
// sessions/ can confuse Claude Code's resume picker, so this is called on
// EVERY restart — not just fresh starts.
func cleanStaleSessions(claudeStateDir string) {
	sessionsDir := filepath.Join(claudeStateDir, "sessions")
	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		// Extract PID from filename: <pid>.json
		pidStr := strings.TrimSuffix(entry.Name(), ".json")
		// Check if the PID corresponds to a running process.
		// On Unix, sending signal 0 checks existence without affecting the process.
		proc, err := os.FindProcess(atoiSafe(pidStr))
		if err != nil || proc.Signal(syscall.Signal(0)) != nil {
			stalePath := filepath.Join(sessionsDir, entry.Name())
			logInit("cleaning stale session PID file: %s", entry.Name())
			_ = os.Remove(stalePath)
		}
	}
}

// atoiSafe converts a string to int, returning -1 on failure (which won't
// match any running PID).
func atoiSafe(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return -1
		}
		n = n*10 + int(c-'0')
	}
	if len(s) == 0 {
		return -1
	}
	return n
}

// maxResumeCandidates caps how many .jsonl files findResumeSession validates
// per call. Only the K most recent by mtime are considered; older sessions
// cannot win the most-recent-valid selection so excluding them from the
// expensive validation/log/stat path keeps boot-path latency O(K) instead of
// O(N). G7 in docs/research/s3-resume-e2e.md sec 5.
const maxResumeCandidates = 512

// findResumeSession returns the path of the latest non-stale Claude session
// log under claudeStateDir/projects/, or "" if no suitable log exists.
// When staleDisable is true, the stale count check is skipped entirely —
// sessions are never considered "too stale to resume".
func findResumeSession(claudeStateDir string, sessionResume bool, staleDisable bool) string {
	if !sessionResume {
		return ""
	}

	projectsDir := filepath.Join(claudeStateDir, "projects")
	if _, err := os.Stat(projectsDir); err != nil {
		return ""
	}

	// When staleness is disabled, skip the stale count check entirely.
	// Sessions are preserved indefinitely for future resume.
	if !staleDisable {
		// Count only recent stale files (last hour) to detect rapid resume
		// failures in the current restart cycle. Old stale files from previous
		// cycles are cleaned up but don't count toward the retry threshold.
		recentStaleCount := 0
		_ = filepath.Walk(projectsDir, func(path string, info os.FileInfo, err error) error {
			if err == nil && strings.HasSuffix(path, ".jsonl.stale") && !info.IsDir() {
				age := time.Since(info.ModTime())
				if age > 24*time.Hour {
					// Clean up old stale files.
					os.Remove(path)
				} else if age < time.Hour {
					// Only count recent stale files toward the retry threshold.
					recentStaleCount++
				}
			}
			return nil
		})
		const maxStaleRetries = 3
		if recentStaleCount >= maxStaleRetries {
			fmt.Printf("[gb agent start] skipping resume: %d recent stale session(s) found (max %d)\n", recentStaleCount, maxStaleRetries)
			return ""
		}
	}

	// Pre-check whether session-env root exists (Claude v2.1+). If it does,
	// we validate each candidate has a matching session-env/<uuid>/ directory.
	sessionEnvRoot := filepath.Join(claudeStateDir, "session-env")
	hasSessionEnvRoot := false
	if fi, err := os.Stat(sessionEnvRoot); err == nil && fi.IsDir() {
		hasSessionEnvRoot = true
	}

	type candidate struct {
		path    string
		modTime time.Time
	}
	type rawEntry struct {
		path string
		info os.FileInfo
	}
	var raw []rawEntry
	totalJSONL, rejSubagent := 0, 0
	_ = filepath.Walk(projectsDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		totalJSONL++
		if strings.Contains(path, "/subagents/") {
			rejSubagent++
			return nil
		}
		raw = append(raw, rawEntry{path, info})
		return nil
	})
	// Bound the validated set to the K most recent .jsonl files (see
	// maxResumeCandidates docstring). Sort descending by mtime, then truncate.
	sort.Slice(raw, func(i, j int) bool {
		return raw[i].info.ModTime().After(raw[j].info.ModTime())
	})
	capped := 0
	if len(raw) > maxResumeCandidates {
		capped = len(raw) - maxResumeCandidates
		raw = raw[:maxResumeCandidates]
	}

	var candidates []candidate
	rejTiny, rejNoEnv := 0, 0
	for _, e := range raw {
		path, info := e.path, e.info
		// Skip tiny transcripts (< 256 bytes) — likely just the init
		// message with no real conversation to resume.
		if info.Size() < 256 {
			fmt.Printf("[gb agent start] resume candidate rejected (too small): %s size=%d modtime=%s\n", path, info.Size(), info.ModTime().Format(time.RFC3339))
			rejTiny++
			continue
		}
		// Claude v2.1+: validate session-env directory exists for this session.
		// session-env/<uuid>/ stores environment state that --resume needs.
		// If the root session-env dir exists (v2.1+ has been used) but this
		// session's entry is missing, the env state was lost — resume will fail.
		if hasSessionEnvRoot {
			sessionID := strings.TrimSuffix(filepath.Base(path), ".jsonl")
			envDir := filepath.Join(sessionEnvRoot, sessionID)
			if envFi, envErr := os.Stat(envDir); envErr != nil || !envFi.IsDir() {
				fmt.Printf("[gb agent start] retiring session %s: no session-env directory (size=%d modtime=%s)\n", filepath.Base(path), info.Size(), info.ModTime().Format(time.RFC3339))
				rejNoEnv++
				_ = os.Rename(path, path+".stale")
				continue
			}
		}
		fmt.Printf("[gb agent start] resume candidate: %s size=%d modtime=%s\n", path, info.Size(), info.ModTime().Format(time.RFC3339))
		candidates = append(candidates, candidate{path, info.ModTime()})
	}
	fmt.Printf("[gb agent start] resume scan: total_jsonl=%d eligible=%d rejected_tiny=%d rejected_subagent=%d rejected_no_env=%d capped=%d\n", totalJSONL, len(candidates), rejTiny, rejSubagent, rejNoEnv, capped)

	if len(candidates) == 0 {
		fmt.Printf("[gb agent start] resume: no eligible session found, selected=\"\"\n")
		return ""
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].modTime.After(candidates[j].modTime)
	})
	best := candidates[0].path
	fmt.Printf("[gb agent start] found session log for resume: %s\n", best)
	// Validate the last line is complete JSON. A partial write from an
	// abrupt kill leaves an incomplete line that breaks --resume.
	truncateIncompleteLastLine(best)
	return best
}

// isStopRequested checks whether the agent bead has stop_requested=true, or the
// local stop sentinel records a stop (see checkStopRequested). This is set by
// 'gb stop' and tells the restart loop to exit instead of restarting. Retries
// up to 3 times on transient errors to avoid accidentally restarting an agent
// that requested a polite stop.
func isStopRequested(ctx context.Context, agentBeadID string) bool {
	state, _ := checkStopRequested(ctx, agentBeadID)
	return state == stopRequested
}

// truncateIncompleteLastLine removes the last line of a .jsonl file if it
// is not valid JSON. This repairs files left with a partial write after an
// abrupt kill (SIGKILL).
func truncateIncompleteLastLine(path string) {
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return
	}
	// Find the last newline before EOF.
	data = bytes.TrimRight(data, "\n")
	lastNL := bytes.LastIndexByte(data, '\n')
	if lastNL < 0 {
		return // single line, nothing to truncate to
	}
	lastLine := data[lastNL+1:]
	if json.Valid(lastLine) {
		return // last line is valid JSON, no truncation needed
	}
	fmt.Printf("[gb agent start] truncating corrupted last line from %s\n", path)
	// Keep everything up to and including the last newline.
	if err := os.WriteFile(path, append(data[:lastNL], '\n'), 0644); err != nil {
		fmt.Printf("[gb agent start] warning: truncate %s: %v\n", path, err)
	}
}

// retireStaleSession renames a session log to .jsonl.stale so it won't be
// resumed on the next restart. Also retires the companion directory and
// session-env directory to prevent orphaned state from accumulating.
func retireStaleSession(logPath string) {
	stalePath := logPath + ".stale"
	if err := os.Rename(logPath, stalePath); err != nil {
		fmt.Printf("[gb agent start] WARNING: could not retire stale session: %v\n", err)
	} else {
		fmt.Printf("[gb agent start] retired stale session: %s -> %s\n", logPath, stalePath)
	}

	// Retire the companion directory (same name minus .jsonl extension).
	companionDir := strings.TrimSuffix(logPath, ".jsonl")
	if fi, err := os.Stat(companionDir); err == nil && fi.IsDir() {
		staleCompanion := companionDir + ".stale"
		if err := os.Rename(companionDir, staleCompanion); err != nil {
			fmt.Printf("[gb agent start] WARNING: could not retire companion dir: %v\n", err)
		}
	}

	// Retire the session-env directory for this session UUID.
	sessionUUID := strings.TrimSuffix(filepath.Base(logPath), ".jsonl")
	claudeDir := filepath.Dir(filepath.Dir(filepath.Dir(logPath))) // projects/<hash>/<uuid>.jsonl → .claude
	sessionEnvDir := filepath.Join(claudeDir, "session-env", sessionUUID)
	if fi, err := os.Stat(sessionEnvDir); err == nil && fi.IsDir() {
		staleEnv := sessionEnvDir + ".stale"
		if err := os.Rename(sessionEnvDir, staleEnv); err != nil {
			fmt.Printf("[gb agent start] WARNING: could not retire session-env: %v\n", err)
		}
	}
}

// cleanAllSessionLogs removes every .jsonl (and .jsonl.stale) under
// projects/ along with companion dirs and session-env/<uuid>/ entries.
// Called when gb init has decided this is a fresh session: Claude Code
// auto-discovers any .jsonl in projects/<hash>/ and shows a resume picker
// regardless of whether --resume was passed, so the only way to keep
// "fresh" actually fresh is to remove the discovery surface entirely
// before the agent starts. See ka-0000004439.
func cleanAllSessionLogs(claudeStateDir string) {
	projectsDir := filepath.Join(claudeStateDir, "projects")
	if _, err := os.Stat(projectsDir); err != nil {
		return
	}
	sessionEnvRoot := filepath.Join(claudeStateDir, "session-env")
	cleaned := 0
	_ = filepath.Walk(projectsDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		name := info.Name()
		if !strings.HasSuffix(name, ".jsonl") && !strings.HasSuffix(name, ".jsonl.stale") {
			return nil
		}
		// Resolve session UUID (filename minus .jsonl[.stale]).
		base := strings.TrimSuffix(name, ".stale")
		uuid := strings.TrimSuffix(base, ".jsonl")
		dir := filepath.Dir(path)

		_ = os.Remove(path)
		// Companion directories live next to the .jsonl, named <uuid>[.stale].
		_ = os.RemoveAll(filepath.Join(dir, uuid))
		_ = os.RemoveAll(filepath.Join(dir, uuid+".stale"))
		// session-env entry for this UUID, if any.
		if uuid != "" {
			_ = os.RemoveAll(filepath.Join(sessionEnvRoot, uuid))
			_ = os.RemoveAll(filepath.Join(sessionEnvRoot, uuid+".stale"))
		}
		cleaned++
		return nil
	})
	if cleaned > 0 {
		logInit("fresh session: removed %d local session log(s) to prevent Claude auto-resume", cleaned)
	}
}

// maxRetainedSessionLogs bounds how many retired (.jsonl.stale) transcripts
// retireAllSessionLogs leaves on disk per projects/ tree.
const maxRetainedSessionLogs = 3

// retireAllSessionLogs is the crash-restart counterpart of cleanAllSessionLogs
// (gd-HPVt3BCPCH): it takes every live .jsonl out of the discovery surface
// WITHOUT deleting the bytes. Each transcript is renamed to .jsonl.stale (with
// its companion dir and session-env entry, via retireStaleSession), so:
//
//   - the local record of a crashed turn survives for forensics/manual recovery
//     instead of being destroyed on every restart, and
//   - nothing named *.jsonl is left for coop's tier-2 session-log discovery
//     (resume.rs discover_session_log: newest-mtime *.jsonl) to latch onto.
//     Leaving the previous incarnation's transcript in place made coop replay it
//     as the live session and drove deterministic FATAL restart loops
//     (gd-2g0LXWF4Sz), and Claude Code's own auto-resume picker only lists
//     *.jsonl, so it cannot fire either.
//
// The conversation itself is resumed from the S3 archive: coop restores it to
// projects/<hash>/<uuid>.jsonl at boot, which is not affected by retiring.
// Only the newest maxRetainedSessionLogs retired transcripts are kept. Returns
// the number of transcripts retired.
func retireAllSessionLogs(claudeStateDir string) int {
	projectsDir := filepath.Join(claudeStateDir, "projects")
	if _, err := os.Stat(projectsDir); err != nil {
		return 0
	}
	var live []string
	_ = filepath.Walk(projectsDir, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && strings.HasSuffix(info.Name(), ".jsonl") {
			live = append(live, path)
		}
		return nil
	})
	for _, path := range live {
		// A same-named retiree from an earlier restart (coop re-places the
		// restored transcript at the same path every boot) would make renaming
		// the companion dir onto it fail; replace it.
		_ = os.RemoveAll(strings.TrimSuffix(path, ".jsonl") + ".stale")
		uuid := strings.TrimSuffix(filepath.Base(path), ".jsonl")
		_ = os.RemoveAll(filepath.Join(claudeStateDir, "session-env", uuid+".stale"))
		retireStaleSession(path)
	}
	if len(live) > 0 {
		logInit("crash restart: retired %d local session log(s) to .jsonl.stale (kept on disk, hidden from coop discovery)", len(live))
	}
	pruneRetiredSessionLogs(projectsDir, maxRetainedSessionLogs)
	return len(live)
}

// pruneRetiredSessionLogs keeps the newest `keep` .jsonl.stale files under
// projectsDir and deletes the rest along with their companion dirs.
func pruneRetiredSessionLogs(projectsDir string, keep int) {
	type stale struct {
		path string
		mod  time.Time
	}
	var all []stale
	_ = filepath.Walk(projectsDir, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && strings.HasSuffix(info.Name(), ".jsonl.stale") {
			all = append(all, stale{path, info.ModTime()})
		}
		return nil
	})
	if len(all) <= keep {
		return
	}
	sort.Slice(all, func(i, j int) bool { return all[i].mod.After(all[j].mod) })
	for _, st := range all[keep:] {
		_ = os.Remove(st.path)
		_ = os.RemoveAll(strings.TrimSuffix(st.path, ".jsonl.stale") + ".stale")
	}
}
