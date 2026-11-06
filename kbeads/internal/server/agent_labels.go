package server

import (
	"encoding/json"
	"strings"
)

// withDerivedAgentLabels returns labels with project:<fields.project> added
// when no project: label is present, and role:<r> added for each
// comma-separated role in fields.role that is not already labelled. Fields are
// read by exact key (exactFieldString); an empty or unparseable field adds
// nothing. The spawn client (beadsapi.Client.SpawnAgentResult) adds the same
// labels after a successful create; deriving them in createBead covers the
// paths that never run that follow-up — the escalation approve replay above
// all — so an agent's project-scoped token can always see its own bead.
func withDerivedAgentLabels(labels []string, fields json.RawMessage) []string {
	if len(fields) == 0 {
		return labels
	}
	out := labels
	if !hasProjectLabel(out) {
		if p := strings.TrimSpace(projectFromFields(fields)); p != "" {
			out = append(out, "project:"+p)
		}
	}
	for _, r := range strings.Split(roleFromFields(fields), ",") {
		r = strings.TrimSpace(r)
		if r == "" || hasLabel(out, "role:"+r) {
			continue
		}
		out = append(out, "role:"+r)
	}
	return out
}
