package hooks

import (
	"context"
	"testing"
	"time"
)

func TestExecuteSuccess(t *testing.T) {
	result := Execute(context.Background(), "echo hello", 5, "", nil)
	if result.Err != nil {
		t.Fatalf("Execute error: %v", result.Err)
	}
	if result.Output != "hello" {
		t.Errorf("Output = %q, want %q", result.Output, "hello")
	}
}

func TestExecuteFailure(t *testing.T) {
	result := Execute(context.Background(), "exit 1", 5, "", nil)
	if result.Err == nil {
		t.Error("expected error from failing command")
	}
}

func TestExecuteStderr(t *testing.T) {
	// When stdout is empty, stderr is used.
	result := Execute(context.Background(), "echo error >&2", 5, "", nil)
	if result.Err != nil {
		t.Fatalf("Execute error: %v", result.Err)
	}
	if result.Output != "error" {
		t.Errorf("Output = %q, want %q (from stderr)", result.Output, "error")
	}
}

func TestExecuteDefaultTimeout(t *testing.T) {
	// timeoutSec <= 0 should use DefaultTimeout (30s), not block forever.
	result := Execute(context.Background(), "echo ok", 0, "", nil)
	if result.Err != nil {
		t.Fatalf("Execute with 0 timeout: %v", result.Err)
	}
	if result.Output != "ok" {
		t.Errorf("Output = %q, want %q", result.Output, "ok")
	}
}

func TestExecuteMaxTimeout(t *testing.T) {
	// Timeout > MaxTimeout should be clamped.
	start := time.Now()
	result := Execute(context.Background(), "echo fast", 999, "", nil)
	elapsed := time.Since(start)
	if result.Err != nil {
		t.Fatalf("Execute error: %v", result.Err)
	}
	if elapsed > 5*time.Second {
		t.Error("command should have completed quickly, not waited for full timeout")
	}
}

func TestExecuteWithCWD(t *testing.T) {
	dir := t.TempDir()
	result := Execute(context.Background(), "pwd", 5, dir, nil)
	if result.Err != nil {
		t.Fatalf("Execute error: %v", result.Err)
	}
	if result.Output != dir {
		t.Errorf("Output = %q, want %q", result.Output, dir)
	}
}

func TestExecuteWithInvalidCWD(t *testing.T) {
	// Invalid CWD should not crash — command runs with default dir.
	result := Execute(context.Background(), "echo ok", 5, "/nonexistent/path/12345", nil)
	if result.Err != nil {
		t.Fatalf("Execute error: %v", result.Err)
	}
	if result.Output != "ok" {
		t.Errorf("Output = %q, want %q", result.Output, "ok")
	}
}

func TestExecuteWithEnv(t *testing.T) {
	env := map[string]string{
		"TEST_VAR": "hello_from_test",
	}
	result := Execute(context.Background(), "echo $TEST_VAR", 5, "", env)
	if result.Err != nil {
		t.Fatalf("Execute error: %v", result.Err)
	}
	if result.Output != "hello_from_test" {
		t.Errorf("Output = %q, want %q", result.Output, "hello_from_test")
	}
}

func TestExecuteContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	result := Execute(ctx, "sleep 10", 30, "", nil)
	if result.Err == nil {
		t.Error("expected error for canceled context")
	}
}

func TestExecuteTimeout(t *testing.T) {
	start := time.Now()
	result := Execute(context.Background(), "sleep 10", 1, "", nil)
	elapsed := time.Since(start)

	if result.Err == nil {
		t.Error("expected error for timed-out command")
	}
	if elapsed > 5*time.Second {
		t.Errorf("should have timed out after ~1s, took %v", elapsed)
	}
}
