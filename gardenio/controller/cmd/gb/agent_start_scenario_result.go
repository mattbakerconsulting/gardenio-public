package main

// Scenario result-file plumbing for `gb agent start --local --test-scenario=...`.
//
// Background (ka-0000006600): The scenario body's bash assertions run inside a
// claudeless tool call. When a body `exit 1`s on assertion failure, that exit
// kills the bash subprocess but does NOT propagate to claudeless or to coop —
// claudeless keeps running, gb-agent later asks coop to shut down, coop SIGTERMs
// the child, and the resulting non-zero coop exit was previously "treated as
// clean" by the scenarioShutdownRequested guard. The net effect: every
// claudeless smoke (lifecycle, prime-fetch, sigterm, …) appeared green even
// when its assertions failed.
//
// Fix: gb-agent allocates a temp result-file path, exports it as
// GB_SCENARIO_RESULT_FILE in the child env, and reads it after shutdown.
// Scenarios MUST write either `pass` (or `0`) for success, or anything else
// (including `fail`, a non-zero numeric, or no file at all) for failure.
//
// Back-compat: a scenario that never writes the file is treated as a failure
// with a clear diagnostic. This is the point — the bug is that the previous
// "no signal at all" silently meant pass. Scenarios that exercise behavior
// without explicit assertions should write `pass` from their final tool turn
// (typically right before `gb stop`).

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// ErrScenarioFailed is returned by readScenarioResult when the scenario body
// reported failure (or did not report at all). runLocal converts this into a
// non-zero exit from `gb agent start`.
var ErrScenarioFailed = errors.New("scenario body reported failure")

// scenarioResultEnv is the env var gb-agent exports to the spawned coop child
// pointing at the temp result file. Public-ish (PascalCase via constant) so
// docs and scenarios can grep for the canonical name.
const scenarioResultEnv = "GB_SCENARIO_RESULT_FILE"

// allocScenarioResultFile creates a fresh temp file for the scenario body to
// write its pass/fail verdict into. Returns the file path plus a cleanup
// closure that removes the file. Cleanup is no-op safe.
func allocScenarioResultFile() (string, func(), error) {
	f, err := os.CreateTemp("", "gb-scenario-result-*.txt")
	if err != nil {
		return "", func() {}, fmt.Errorf("allocate scenario result file: %w", err)
	}
	path := f.Name()
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return "", func() {}, fmt.Errorf("close scenario result file: %w", err)
	}
	cleanup := func() { _ = os.Remove(path) }
	return path, cleanup, nil
}

// readScenarioResult inspects the result file written by the scenario body
// and returns nil for a pass or ErrScenarioFailed (wrapped with diagnostic
// context) for any other state.
//
// Pass conditions:
//   - file exists, first non-blank line == "pass" (case-insensitive), OR
//   - file exists, first non-blank line == "0" (the body's $? after success)
//
// Failure conditions (anything else) include:
//   - file missing (scenario did not report — see package doc)
//   - file empty
//   - first line == "fail", "FAIL", non-zero numeric, or any other token
//
// path may be "" (called outside scenario mode); in that case the function
// returns nil — there's no result to evaluate.
func readScenarioResult(path string) error {
	if path == "" {
		return nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%w: scenario did not write %s (the body must emit `pass` or `fail`)", ErrScenarioFailed, scenarioResultEnv)
		}
		return fmt.Errorf("read scenario result file %s: %w", path, err)
	}
	verdict := firstNonBlankLine(raw)
	switch strings.ToLower(verdict) {
	case "pass", "0":
		return nil
	case "":
		return fmt.Errorf("%w: %s is empty", ErrScenarioFailed, path)
	default:
		return fmt.Errorf("%w: %s reported %q", ErrScenarioFailed, path, verdict)
	}
}

// firstNonBlankLine returns the first non-empty, non-comment line of `raw`
// trimmed of surrounding whitespace. Lines beginning with `#` are skipped so
// scenarios can comment the file body. Empty input returns "".
func firstNonBlankLine(raw []byte) string {
	for line := range strings.SplitSeq(string(raw), "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		return t
	}
	return ""
}
