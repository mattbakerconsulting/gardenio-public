package beadsapi

import "strings"

// MuxSessionID is the id coopmux knows this agent's session by, "" when the
// bead names none.
//
// In a pod, coop registers with coopmux under the pod name: the agent image's
// entrypoint exports COOP_MUX_SESSION_ID="$HOSTNAME" so that #<pod> links and
// Slack deep links resolve (coop/crates/cli/src/run.rs), and the status
// reporter writes that same name to the bead's notes as pod_name. The bead's
// last_session_id field is coop's own session id, carried across resumes; it is
// what coopmux registers only when nothing overrides it (an agent outside a
// pod). Reading last_session_id alone therefore asked coopmux for a session it
// never had for any pod-run agent, and every Glass answered "not live"
// (gd-oJOJvsOdw7). So: pod_name first, then last_session_id.
func (b *BeadDetail) MuxSessionID() string {
	if b == nil {
		return ""
	}
	if pod := strings.TrimSpace(ParseNotes(b.Notes)["pod_name"]); pod != "" {
		return pod
	}
	return strings.TrimSpace(b.Fields["last_session_id"])
}
