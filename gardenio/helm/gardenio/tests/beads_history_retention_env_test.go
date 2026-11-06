package tests

import (
	"strings"
	"testing"
)

// BEADS_HISTORY_RETENTION_DAYS sets the kbeads history-retention sweep window
// (kbeads/internal/server/history_retention.go, gd-1F8R0SQtiQ). The chart owns
// it through beads.historyRetentionDays and always renders it, so an explicit 0
// (the off switch) reaches the daemon instead of being dropped or defaulted.

const historyRetentionEnvName = "- name: BEADS_HISTORY_RETENTION_DAYS"

func TestBeadsHistoryRetentionEnv(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{name: "default", want: "14"},
		{name: "disabled", args: []string{"--set", "beads.historyRetentionDays=0"}, want: "0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lines := strings.Split(helmBeadsDeployment(t, tc.args...), "\n")
			for i, l := range lines {
				if strings.TrimSpace(l) != historyRetentionEnvName {
					continue
				}
				if want := `value: "` + tc.want + `"`; i+1 >= len(lines) || strings.TrimSpace(lines[i+1]) != want {
					t.Fatalf("BEADS_HISTORY_RETENTION_DAYS must be followed by %s; got:\n%s", want, strings.Join(lines[i:min(i+2, len(lines))], "\n"))
				}
				return
			}
			t.Fatalf("expected BEADS_HISTORY_RETENTION_DAYS in render, not found")
		})
	}
}
