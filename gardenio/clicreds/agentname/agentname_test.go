package agentname

import "testing"

func TestEscalationExecutor(t *testing.T) {
	for _, tc := range []struct{ prefix, id, want string }{
		{EscalationExecutorPrefix, "gd-bH4HHHzucm", "escalation-gd-bh4hhhzucm"},
		{ServiceAccountExecutorPrefix, "gd-JxfEHn2INg", "escalation-sa-gd-jxfehn2ing"},
		{EscalationExecutorPrefix, "gd-esc2", "escalation-gd-esc2"},
	} {
		if got := EscalationExecutor(tc.prefix, tc.id); got != tc.want {
			t.Errorf("EscalationExecutor(%q, %q) = %q, want %q", tc.prefix, tc.id, got, tc.want)
		}
	}
}
