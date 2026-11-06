package gardenhub

import "testing"

// TestAgentLaneHolder: a non-floor escalation an agent lane holds names that lane; a floor one, one the owner or a land
// owner holds, or an unstamped one names nobody, so its surfaces keep Approve as the primary action (gd-Mq0uDXHswK).
func TestAgentLaneHolder(t *testing.T) {
	merge := map[string]string{"operation_type": "gitlab_mr_merge", "audience": "desk", "held_by": "coordinator:merge-desk",
		"approvers": `["coordinator:merge-desk","coordinator:laptop","operator"]`}
	with := func(k, v string) map[string]string {
		f := map[string]string{}
		for a, b := range merge {
			f[a] = b
		}
		f[k] = v
		return f
	}
	for name, c := range map[string]struct {
		f    map[string]string
		want string
	}{
		"desk-held merge":         {merge, "the merge desk"},
		"laptop-held":             {with("held_by", "coordinator:laptop"), "coordinator laptop"},
		"steward-held":            {with("held_by", "steward:acme"), "the acme steward"},
		"owner-held":              {with("held_by", "operator"), ""},
		"land owner-held":         {with("held_by", "land-owner:acme"), ""},
		"unstamped":               {with("held_by", ""), ""},
		"bare coordinator":        {with("held_by", "coordinator:"), ""},
		"floor operation":         {with("operation_type", "credential_rotation"), ""},
		"operator_only":           {with("operator_only", "true"), ""},
		"secret":                  {with("sensitivity", "secret"), ""},
		"a person among approver": {with("approvers", `["coordinator:merge-desk","oidc:a@x.example"]`), ""},
	} {
		if got := AgentLaneHolder(c.f); got != c.want {
			t.Errorf("%s: AgentLaneHolder = %q, want %q", name, got, c.want)
		}
	}
}
