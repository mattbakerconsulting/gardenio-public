package main

// Pre-process claudeless scenario files for `gb agent start --local`.
//
// Local-mode E2E scenarios (gardenio/tests/e2e/claudeless/local-*.toml) embed
// a gardenio-side `[test_scenario]` table that carries the startup_prompt the
// runner injects via coop. claudeless 0.4.0 strict-parses scenarios and
// rejects any unknown top-level table, so handing the file to claudeless
// verbatim makes the inner agent exit code 1 ~500 ms after launch — long
// before injectLocalStartupPrompt can deliver anything (ka-0000002111).
//
// materializeClaudelessScenario writes a stripped copy with the
// `[test_scenario]` table removed and returns the temp path. Scenarios that
// don't declare the table are returned as-is.

import (
	"bytes"
	"fmt"
	"os"
)

// materializeClaudelessScenario returns a path safe to hand to claudeless's
// `--scenario` flag. When src contains a `[test_scenario]` table (or any
// `[test_scenario.<sub>]` subtable), the function writes a copy with those
// lines stripped to a temp file and returns its path plus a cleanup closure.
//
// When src has no `[test_scenario]` table the function returns src unchanged
// and a no-op cleanup. Callers should always defer the cleanup; passing a
// no-op is harmless.
func materializeClaudelessScenario(src string) (string, func(), error) {
	raw, err := os.ReadFile(src)
	if err != nil {
		return "", func() {}, fmt.Errorf("read scenario %s: %w", src, err)
	}
	stripped, removed := stripTestScenarioTable(raw)
	if !removed {
		return src, func() {}, nil
	}
	tmp, err := os.CreateTemp("", "gb-claudeless-scenario-*.toml")
	if err != nil {
		return "", func() {}, fmt.Errorf("create temp scenario: %w", err)
	}
	if _, err := tmp.Write(stripped); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", func() {}, fmt.Errorf("write temp scenario: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return "", func() {}, fmt.Errorf("close temp scenario: %w", err)
	}
	cleanup := func() { _ = os.Remove(tmp.Name()) }
	return tmp.Name(), cleanup, nil
}

// stripTestScenarioTable removes the `[test_scenario]` table — and any
// `[test_scenario.<sub>]` subtables — from a TOML document. It is a
// line-based pass: lines under such a header are skipped until the next
// top-level `[...]` or `[[...]]` header. Returns the stripped bytes and a
// flag indicating whether anything was removed.
//
// The function does not understand multi-line strings or comments that
// happen to contain a `[` at column 0; the local-* scenarios this guards
// don't use either pattern before their tables, so the simpler scanner is
// sufficient.
func stripTestScenarioTable(raw []byte) ([]byte, bool) {
	var out bytes.Buffer
	out.Grow(len(raw))
	skip := false
	removed := false
	for _, line := range bytes.SplitAfter(raw, []byte{'\n'}) {
		trimmed := bytes.TrimLeft(line, " \t")
		if len(trimmed) > 0 && trimmed[0] == '[' {
			if isTestScenarioHeader(trimmed) {
				skip = true
				removed = true
			} else {
				skip = false
			}
		}
		if !skip {
			out.Write(line)
		}
	}
	return out.Bytes(), removed
}

// isTestScenarioHeader reports whether trimmed (a line with leading
// whitespace already removed) opens a `[test_scenario]` or
// `[test_scenario.<sub>]` table. Array-of-tables headers (`[[...]]`) are
// not matched — `[[test_scenario]]` is exotic and would mean something
// different to TOML parsers anyway.
func isTestScenarioHeader(trimmed []byte) bool {
	rest, ok := bytes.CutPrefix(trimmed, []byte("[test_scenario"))
	if !ok {
		return false
	}
	if len(rest) == 0 {
		return false
	}
	switch rest[0] {
	case ']', '.':
		return true
	default:
		return false
	}
}
