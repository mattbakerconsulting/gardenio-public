// Package linttestdata is a fixture exercised by lint_bridge_bead_ids_test.go.
//
// This file is in testdata/ and is therefore ignored by the Go toolchain
// (`go build`, `go test`, `golangci-lint`). It is intentionally a violation
// of the bridge bead-ID lint guard: TestLintScriptFlagsViolationFixture
// runs ./scripts/lint-bridge-bead-ids.sh against this file and expects a
// non-zero exit.
//
// DO NOT "fix" the violations below — they are the test input.

package linttestdata

import "fmt"

type beadEvent struct {
	ID    string
	Title string
}

// renderViolatingDecisionContext interpolates a bare bead.ID inside a
// backtick-wrapped Slack code span. This is exactly the pattern the lint
// guard is meant to catch.
func renderViolatingDecisionContext(bead beadEvent) string {
	return fmt.Sprintf("Decision: `%s`", bead.ID)
}
