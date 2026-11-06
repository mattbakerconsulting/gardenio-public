package commons

import (
	"strings"
	"testing"
)

// A thread's head names it (Commons on a phone M2): who began it and the first
// line of what they said, or of the face that stands in for it.
func TestThreadSubject(t *testing.T) {
	for name, c := range map[string]struct {
		r    row
		want string
	}{
		"text":           {row{M: Message{Author: "oidc:matt@example.com", Text: "Porch chat\nshould open"}}, "matt: Porch chat"},
		"files":          {row{M: Message{Author: "agent:crew-1", Files: []File{{Name: "farm.png"}}}}, "crew-1: farm.png"},
		"agent":          {row{M: Message{Author: "agent:crew-3", Text: "wrapped up"}, Agent: &AgentCard{Name: "crew-3", Task: "Fix the  porch"}}, "crew-3: Fix the porch"},
		"agent, no task": {row{M: Message{Author: "agent:crew-3"}, Agent: &AgentCard{Name: "crew-3", State: "died"}}, "crew-3: died"},
		"decision":       {row{M: Message{Author: "agent:crew-3"}, Decision: &DecisionCard{Prompt: "Ship it?"}}, "crew-3: Ship it?"},
		"death":          {row{M: Message{Author: "agent:reaper", Notice: &Notice{Kind: "death", Agent: "crew-3"}}}, "reaper: Agent died · crew-3"},
		"storm":          {row{M: Message{Author: "agent:reaper", Notice: &Notice{Kind: "death_storm", Count: 7}}}, "reaper: Death storm"},
		"deleted":        {row{M: Message{Author: "oidc:matt@example.com", Text: "gone", Deleted: true}}, "matt: Message deleted"},
		"no words":       {row{M: Message{Author: "oidc:matt@example.com"}}, "matt"},
		"long card":      {row{M: Message{Author: "agent:a"}, Decision: &DecisionCard{Prompt: strings.Repeat("why ", 60)}}, "a: " + strings.Repeat("why ", 29) + "…"},
	} {
		if got := c.r.Subject(); got != c.want {
			t.Errorf("%s: Subject = %q, want %q", name, got, c.want)
		}
	}
}
