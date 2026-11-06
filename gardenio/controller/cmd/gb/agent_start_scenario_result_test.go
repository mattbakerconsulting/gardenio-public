package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// ka-0000006600 regression coverage. Without these, a scenario body that
// `exit 1`s after assertion failure is silently treated as clean by gb-agent
// because coop's teardown exit code is also 1.
func TestReadScenarioResult(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	write := func(name, body string) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	cases := []struct {
		name    string
		path    string
		wantErr bool
	}{
		{name: "empty path is a no-op", path: "", wantErr: false},
		{name: "pass verdict", path: write("pass.txt", "pass\n"), wantErr: false},
		{name: "PASS uppercase", path: write("pass-upper.txt", "PASS\n"), wantErr: false},
		{name: "exit zero", path: write("zero.txt", "0\n"), wantErr: false},
		{name: "pass with leading comment", path: write("commented.txt", "# notes\npass\n"), wantErr: false},
		{name: "fail verdict", path: write("fail.txt", "fail\n"), wantErr: true},
		{name: "non-zero exit code", path: write("one.txt", "1\n"), wantErr: true},
		{name: "garbage verdict", path: write("garbage.txt", "??\n"), wantErr: true},
		{name: "empty file", path: write("empty.txt", ""), wantErr: true},
		{name: "whitespace-only file", path: write("ws.txt", "   \n\n"), wantErr: true},
		{name: "missing file", path: filepath.Join(dir, "missing.txt"), wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := readScenarioResult(tc.path)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				if !errors.Is(err, ErrScenarioFailed) {
					t.Fatalf("expected ErrScenarioFailed, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestAllocScenarioResultFile(t *testing.T) {
	path, cleanup, err := allocScenarioResultFile()
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	defer cleanup()

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected result file to exist at %s: %v", path, err)
	}

	// The freshly allocated file is empty — readScenarioResult must treat
	// that as a failure (the scenario body didn't write a verdict yet).
	if err := readScenarioResult(path); !errors.Is(err, ErrScenarioFailed) {
		t.Fatalf("empty file: expected ErrScenarioFailed, got %v", err)
	}

	// Writing "pass" flips the verdict.
	if err := os.WriteFile(path, []byte("pass\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := readScenarioResult(path); err != nil {
		t.Fatalf("pass: %v", err)
	}

	cleanup()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("cleanup did not remove file: stat err=%v", err)
	}
}

// Mirrors the adversarial fixture invariant from the bug DoD: a scenario body
// that unconditionally signals failure MUST surface as a failure verdict.
// This is the unit-level analogue of the local-adversarial-fail.toml fixture
// — the integration test runs through coop+claudeless, this one pins the
// parsing contract.
func TestReadScenarioResultAdversarialBody(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "verdict.txt")

	// Simulate the bash trailer: `printf '%s\n' "$([ $FAIL -eq 0 ] && echo pass || echo fail)" > "$GB_SCENARIO_RESULT_FILE"`.
	if err := os.WriteFile(path, []byte("fail\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := readScenarioResult(path)
	if !errors.Is(err, ErrScenarioFailed) {
		t.Fatalf("adversarial body: expected ErrScenarioFailed, got %v", err)
	}
}
