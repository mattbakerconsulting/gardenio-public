package tests

import (
	"strings"
	"testing"
)

// CloudWatch runtime-logging env wiring for the lambda_microvm backend on the
// agent-controller deployment (task ka-0000002120, epic ka-0000003545).
//
// The C7 POC run produced NO MicroVM stdout/stderr because RunMicrovmInput's
// Logging member was never set; the controller now sets it from
// MICROVM_LOG_GROUP / MICROVM_LOG_STREAM_PREFIX. These tests pin the chart
// contract:
//   - microvm.enabled=false renders no MICROVM_* env at all (unchanged),
//   - a log group only renders when explicitly configured (logging stays a
//     deliberate opt-in — the backend leaves Logging unset without it),
//   - the stream prefix renders only alongside a group (it is meaningless on
//     its own and must never appear independently).

const microvmTpl = "templates/controller/deployment.yaml"

// TestMicrovmLogging_DefaultOff: enabling the microvm backend without a log
// group must render neither logging env var — the backend then leaves
// RunMicrovm's Logging member unset (the pre-logging behavior).
// (microvmBaseFlags, shared with the git-credential tests, satisfies the
// `required` microvm values so each test only varies the logging knobs.)
func TestMicrovmLogging_DefaultOff(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, microvmTpl, microvmBaseFlags()...)
	for _, forbidden := range []string{"MICROVM_LOG_GROUP", "MICROVM_LOG_STREAM_PREFIX"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("%s must NOT render without agents.microvm.logGroup; got:\n%s", forbidden, out)
		}
	}
}

// TestMicrovmLogging_GroupOnly: a log group without an explicit prefix renders
// only MICROVM_LOG_GROUP; the controller supplies the "agent/" prefix default.
func TestMicrovmLogging_GroupOnly(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, microvmTpl, microvmBaseFlags(
		"agents.microvm.logGroup=/aws/lambda/microvms/ziti-microvm-poc")...)
	for _, want := range []string{
		"name: MICROVM_LOG_GROUP",
		`value: "/aws/lambda/microvms/ziti-microvm-poc"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("logGroup set must render %q; got:\n%s", want, out)
		}
	}
	if strings.Contains(out, "MICROVM_LOG_STREAM_PREFIX") {
		t.Errorf("MICROVM_LOG_STREAM_PREFIX must NOT render without an explicit prefix; got:\n%s", out)
	}
}

// TestMicrovmLogging_GroupAndPrefix: both knobs render verbatim when set.
func TestMicrovmLogging_GroupAndPrefix(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, microvmTpl, microvmBaseFlags(
		"agents.microvm.logGroup=/aws/lambda/microvms/ziti-microvm-poc",
		"agents.microvm.logStreamPrefix=microvm-agent/")...)
	for _, want := range []string{
		"name: MICROVM_LOG_GROUP",
		`value: "/aws/lambda/microvms/ziti-microvm-poc"`,
		"name: MICROVM_LOG_STREAM_PREFIX",
		`value: "microvm-agent/"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("logGroup+logStreamPrefix set must render %q; got:\n%s", want, out)
		}
	}
}

// TestMicrovmLogging_PrefixWithoutGroupInert: a prefix WITHOUT a group renders
// nothing — the value is nested under logGroup in the template so it cannot
// leak out on its own.
func TestMicrovmLogging_PrefixWithoutGroupInert(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, microvmTpl, microvmBaseFlags(
		"agents.microvm.logStreamPrefix=orphan/")...)
	if strings.Contains(out, "MICROVM_LOG_STREAM_PREFIX") {
		t.Errorf("MICROVM_LOG_STREAM_PREFIX must NOT render without logGroup; got:\n%s", out)
	}
}
