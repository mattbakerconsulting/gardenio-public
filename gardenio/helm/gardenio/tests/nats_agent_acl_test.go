package tests

import (
	"strings"
	"testing"
)

// natsSubjectIsolationConfig renders the NATS statefulset with subject
// isolation on and returns the embedded nats-server.conf.
func natsSubjectIsolationConfig(t *testing.T) string {
	t.Helper()
	return helmTemplate(t, "templates/nats/statefulset.yaml",
		"nats.enabled=true",
		"nats.auth.enabled=true",
		"nats.auth.subjectIsolation.enabled=true",
	)
}

// TestNatsAgentUserAllowsRelaySubjects guards the outbound-only registration
// path for MicroVM satellites (task ka-0000006995): a session reached only over
// the Ziti overlay registers by PUBLISHING coop.mux.session.<id>.announce (and
// status/state/usage) and receives commands by SUBSCRIBING to its
// input/nudge/respond subjects. If the agent NATS user's ACL drops these, an
// authenticated MicroVM agent connects successfully but is invisible to
// coopmux and unreachable — a silent regression only a live E2E would catch.
func TestNatsAgentUserAllowsRelaySubjects(t *testing.T) {
	t.Parallel()
	out := natsSubjectIsolationConfig(t)

	// Publish side: the relay prefix wildcard (must cover the coopmux
	// --nats-relay-prefix default "coop.mux").
	if !strings.Contains(out, `"coop.mux.session.>"`) {
		t.Error("agent user must be allowed to publish coop.mux.session.> " +
			"(outbound-only MicroVM registration relay, ka-0000006995)")
	}

	// Subscribe side: exactly the three command verbs — not the full
	// coop.mux.session.> tree, which would let one agent snoop sibling
	// announce/status/state traffic.
	for _, subj := range []string{
		`"coop.mux.session.*.input"`,
		`"coop.mux.session.*.nudge"`,
		`"coop.mux.session.*.respond"`,
	} {
		if !strings.Contains(out, subj) {
			t.Errorf("agent user must be allowed to subscribe %s (coopmux command channel for outbound-only sessions)", subj)
		}
	}
	if strings.Contains(out, `subscribe`) && strings.Contains(out, `"coop.mux.session.>"`) {
		// The broad wildcard must appear only once (publish allow). Count to be
		// sure it did not leak into the subscribe allow.
		if strings.Count(out, `"coop.mux.session.>"`) != 1 {
			t.Error("coop.mux.session.> must appear only in the publish allow, never subscribe (cross-agent snooping)")
		}
	}

	// The pre-existing pod-path allows must survive.
	if !strings.Contains(out, `"coop.events.>"`) {
		t.Error("agent user must keep the coop.events.> publish allow (pod session events)")
	}
}
