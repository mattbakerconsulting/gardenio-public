package main

// Local-mode (gb agent start --local) startup guards for Claude Code
// (gd-bJeLOYSWZG):
//
//   - preTrustClaudeDir marks --dir trusted in the operator's ~/.claude.json so
//     Claude Code never renders the per-folder trust dialog. Pods get the same
//     from writeOnboardingSkip; on a laptop the dialog was the first thing a new
//     user saw, nobody answered it, and claude exited 1.
//   - stopCoopWhenAgentExits asks coop to shut down once the agent process has
//     exited. coop otherwise waits for a credential switch or shutdown forever,
//     so runLocal never returned, and the registry entry and agent bead stayed
//     ALIVE for a dead agent.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"gardenio/controller/internal/coopapi"
)

// preTrustClaudeDir sets projects[dir].hasTrustDialogAccepted in
// ~/.claude.json, for dir and its symlink-resolved path (Claude Code keys
// projects by its getcwd, which is the resolved one). Unlike writeOnboardingSkip
// this is the operator's own file: it touches only those entries, writes
// nothing when they are already trusted, keeps the file mode, and refuses to
// rewrite a file it cannot parse.
func preTrustClaudeDir(dir string) error {
	dirs := []string{dir}
	if resolved, err := filepath.EvalSymlinks(dir); err == nil && resolved != dir {
		dirs = append(dirs, resolved)
	}

	path := filepath.Join(homeDir(), ".claude.json")
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved // keep a dotfiles symlink pointing at the real file
	}
	mode := fs.FileMode(0o600)
	cfg := map[string]any{}
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := json.Unmarshal(data, &cfg); err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
		if cfg == nil {
			cfg = map[string]any{}
		}
		if fi, err := os.Stat(path); err == nil {
			mode = fi.Mode().Perm()
		}
	case errors.Is(err, fs.ErrNotExist):
	default:
		return fmt.Errorf("read %s: %w", path, err)
	}

	projects, _ := cfg["projects"].(map[string]any)
	if projects == nil {
		projects = map[string]any{}
	}
	changed := false
	for _, d := range dirs {
		entry, _ := projects[d].(map[string]any)
		if entry == nil {
			entry = map[string]any{}
		}
		if entry["hasTrustDialogAccepted"] == true {
			continue
		}
		entry["hasTrustDialogAccepted"] = true
		projects[d] = entry
		changed = true
	}
	if !changed {
		return nil
	}
	cfg["projects"] = projects

	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal %s: %w", path, err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".claude.json.gb-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(out, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	fmt.Printf("[gb agent start] Pre-trusted %s in %s (Claude Code folder-trust dialog)\n", dir, path)
	return nil
}

// localAgentExitClient is the slice of coopapi.Client stopCoopWhenAgentExits
// uses (a fake in tests).
type localAgentExitClient interface {
	GetAgentState(ctx context.Context) (*coopapi.AgentState, error)
	GetSessionStatus(ctx context.Context) (*coopapi.SessionStatus, error)
	Shutdown(ctx context.Context) error
}

// localAgentExitPoll is how often stopCoopWhenAgentExits polls coop.
const localAgentExitPoll = 2 * time.Second

// stopCoopWhenAgentExits polls coop until the agent reports "exited", then
// requests a coop shutdown so runLocal's coopProc.Wait returns and its defers
// deregister the agent and close its bead. Returns true when it shut coop down,
// false when ctx ended first. Mirrors gb init's monitor loop in pods.
func stopCoopWhenAgentExits(ctx context.Context, coop localAgentExitClient, interval time.Duration) bool {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
		}
		state, err := coop.GetAgentState(ctx)
		if err != nil || state.State != "exited" {
			continue
		}
		code := "unknown"
		if st, err := coop.GetSessionStatus(ctx); err == nil && st.ExitCode != nil {
			code = fmt.Sprint(*st.ExitCode)
		}
		fmt.Fprintf(os.Stderr, "[gb agent start] agent exited (code=%s); shutting down coop\n", code)
		sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
		err = coop.Shutdown(sctx)
		scancel()
		if err != nil {
			fmt.Fprintf(os.Stderr, "[gb agent start] warning: coop shutdown after agent exit failed: %v\n", err)
		}
		return true
	}
}
