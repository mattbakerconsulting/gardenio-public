package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnqueuePrimeContent_WritesJSONLEntry(t *testing.T) {
	tmpDir := t.TempDir()
	queuePath := filepath.Join(tmpDir, "inject-queue.jsonl")

	old := injectQueueFile
	injectQueueFile = queuePath
	t.Cleanup(func() { injectQueueFile = old })

	content := "<system-reminder>\n## Test Prime\n</system-reminder>\n"
	if err := enqueuePrimeContent(content); err != nil {
		t.Fatalf("enqueuePrimeContent() error: %v", err)
	}

	data, err := os.ReadFile(queuePath)
	if err != nil {
		t.Fatalf("reading queue file: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected 1 JSONL line, got %d", len(lines))
	}

	var entry map[string]string
	if err := json.Unmarshal([]byte(lines[0]), &entry); err != nil {
		t.Fatalf("unmarshalling JSONL entry: %v", err)
	}

	if entry["content"] != content {
		t.Errorf("content mismatch:\ngot:  %q\nwant: %q", entry["content"], content)
	}
}

func TestEnqueuePrimeContent_AppendsToExisting(t *testing.T) {
	tmpDir := t.TempDir()
	queuePath := filepath.Join(tmpDir, "inject-queue.jsonl")

	old := injectQueueFile
	injectQueueFile = queuePath
	t.Cleanup(func() { injectQueueFile = old })

	existing := `{"content":"existing entry"}` + "\n"
	if err := os.WriteFile(queuePath, []byte(existing), 0644); err != nil {
		t.Fatalf("writing seed file: %v", err)
	}

	if err := enqueuePrimeContent("new prime context"); err != nil {
		t.Fatalf("enqueuePrimeContent() error: %v", err)
	}

	data, err := os.ReadFile(queuePath)
	if err != nil {
		t.Fatalf("reading queue file: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 JSONL lines (existing + new), got %d", len(lines))
	}

	if lines[0] != `{"content":"existing entry"}` {
		t.Errorf("first line should be existing entry, got: %s", lines[0])
	}

	var entry map[string]string
	if err := json.Unmarshal([]byte(lines[1]), &entry); err != nil {
		t.Fatalf("unmarshalling second JSONL entry: %v", err)
	}
	if entry["content"] != "new prime context" {
		t.Errorf("second entry content = %q, want %q", entry["content"], "new prime context")
	}
}

func TestEnqueuePrimeContent_EmptyContentSkipped(t *testing.T) {
	// The hookPrimeCmd handler skips enqueue when content is empty.
	// Verify enqueuePrimeContent itself still works (it writes empty),
	// but the guard in the command handler prevents the call.
	tmpDir := t.TempDir()
	queuePath := filepath.Join(tmpDir, "inject-queue.jsonl")

	old := injectQueueFile
	injectQueueFile = queuePath
	t.Cleanup(func() { injectQueueFile = old })

	// Simulate the guard: primeEnqueue && primeContent != ""
	primeContent := ""
	if primeContent != "" {
		if err := enqueuePrimeContent(primeContent); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}

	if _, err := os.Stat(queuePath); !os.IsNotExist(err) {
		t.Error("queue file should not exist when prime content is empty")
	}
}

func TestEnqueuePrimeContent_JQCompatibleFormat(t *testing.T) {
	// Verify the JSONL output is compatible with drain-queue.sh's
	// jq -r '.content // empty' extraction.
	tmpDir := t.TempDir()
	queuePath := filepath.Join(tmpDir, "inject-queue.jsonl")

	old := injectQueueFile
	injectQueueFile = queuePath
	t.Cleanup(func() { injectQueueFile = old })

	content := "## Workflow Context\nLine two with special chars: \"quotes\" & <tags>"
	if err := enqueuePrimeContent(content); err != nil {
		t.Fatalf("enqueuePrimeContent() error: %v", err)
	}

	// Verify jq can extract the content field (if jq is available).
	jqPath, err := exec.LookPath("jq")
	if err != nil {
		t.Skip("jq not available, skipping jq compatibility check")
	}

	cmd := exec.Command(jqPath, "-r", ".content // empty", queuePath)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("jq extraction failed: %v", err)
	}

	extracted := strings.TrimSuffix(string(out), "\n")
	if extracted != content {
		t.Errorf("jq extracted content mismatch:\ngot:  %q\nwant: %q", extracted, content)
	}
}

func TestEnqueuePrimeContent_MultilineContent(t *testing.T) {
	// Verify multiline content (typical prime output) roundtrips correctly.
	tmpDir := t.TempDir()
	queuePath := filepath.Join(tmpDir, "inject-queue.jsonl")

	old := injectQueueFile
	injectQueueFile = queuePath
	t.Cleanup(func() { injectQueueFile = old })

	content := "<system-reminder>\n## Header\n\n- Rule 1\n- Rule 2\n\n## Advice\n\nDo the thing.\n</system-reminder>\n"
	if err := enqueuePrimeContent(content); err != nil {
		t.Fatalf("enqueuePrimeContent() error: %v", err)
	}

	data, err := os.ReadFile(queuePath)
	if err != nil {
		t.Fatalf("reading queue file: %v", err)
	}

	// Should be exactly 1 JSONL line (no embedded newlines breaking the format).
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 1 {
		t.Fatalf("multiline content should produce exactly 1 JSONL line, got %d", len(lines))
	}

	var entry map[string]string
	if err := json.Unmarshal([]byte(lines[0]), &entry); err != nil {
		t.Fatalf("unmarshalling entry: %v", err)
	}

	if entry["content"] != content {
		t.Errorf("roundtrip content mismatch:\ngot:  %q\nwant: %q", entry["content"], content)
	}
}
