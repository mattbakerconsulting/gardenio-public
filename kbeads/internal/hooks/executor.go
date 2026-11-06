// Package hooks provides advice hook execution for session lifecycle events.
package hooks

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// Default and max timeout for hook commands.
const (
	DefaultTimeout = 30 * time.Second
	MaxTimeout     = 300 * time.Second
)

// Result holds the output of running a single hook command.
type Result struct {
	Output string
	Err    error
}

// Execute runs a shell command with the given timeout and environment.
// The command is executed via "sh -c" in the specified working directory.
func Execute(ctx context.Context, command string, timeoutSec int, cwd string, env map[string]string) Result {
	timeout := time.Duration(timeoutSec) * time.Second
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	if timeout > MaxTimeout {
		timeout = MaxTimeout
	}

	hookCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(hookCtx, "sh", "-c", command) //nolint:gosec // hook commands are from trusted advice config

	// Use a process group so we can kill the entire tree on timeout.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		// Kill the entire process group (negative PID).
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 2 * time.Second

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if cwd != "" {
		if info, err := os.Stat(cwd); err == nil && info.IsDir() {
			cmd.Dir = cwd
		}
	}

	// Inherit process environment and overlay hook-specific vars.
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}

	err := cmd.Run()
	output := strings.TrimSpace(stdout.String())
	if output == "" {
		output = strings.TrimSpace(stderr.String())
	}

	return Result{Output: output, Err: err}
}
