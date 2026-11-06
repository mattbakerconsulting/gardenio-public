package commons

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

func TestNoticeFrom(t *testing.T) {
	for name, c := range map[string]struct {
		fields  map[string]string
		subject string
		want    *Notice
	}{
		"death": {
			fields:  map[string]string{"notice": "death", "notice_agent": "crew-1", "notice_cause": "pod deleted"},
			subject: "gd-a1",
			want:    &Notice{Kind: "death", Agent: "crew-1", Subject: "gd-a1", Cause: "pod deleted"},
		},
		"crash names its subject when it has no name": {
			fields:  map[string]string{"notice": "crash", "notice_pod": "p-1", "notice_pod_phase": "failed"},
			subject: "gd-a1",
			want:    &Notice{Kind: "crash", Agent: "gd-a1", Subject: "gd-a1", Pod: "p-1", Phase: "failed"},
		},
		"storm": {
			fields: map[string]string{"notice": "death_storm", "notice_count": "4", "notice_window": "5m0s",
				"notice_dead": `[{"id":"gd-a1","name":"crew-1"},{"id":"gd-a2"},{"name":""}]`},
			want: &Notice{Kind: "death_storm", Count: 4, Window: "5 minutes",
				Dead: []NoticeAgent{{ID: "gd-a1", Name: "crew-1"}, {ID: "gd-a2", Name: "gd-a2"}}, More: 2},
		},
		"storm count is at least the deaths it names": {
			fields: map[string]string{"notice": "death_storm", "notice_dead": `[{"id":"gd-a1","name":"crew-1"}]`},
			want:   &Notice{Kind: "death_storm", Count: 1, Dead: []NoticeAgent{{ID: "gd-a1", Name: "crew-1"}}},
		},
		"empty storm":           {fields: map[string]string{"notice": "death_storm", "notice_count": "0"}},
		"death with no agent":   {fields: map[string]string{"notice": "death"}},
		"plain notice":          {fields: map[string]string{"kind": "notice"}},
		"unknown notice":        {fields: map[string]string{"notice": "jack"}, subject: "gd-a1"},
		"unreadable dead is ok": {fields: map[string]string{"notice": "death_storm", "notice_count": "2", "notice_dead": "{"}, want: &Notice{Kind: "death_storm", Count: 2, More: 2}},
	} {
		t.Run(name, func(t *testing.T) {
			if c.subject != "" {
				c.fields[fieldNoticeSubject] = c.subject
			}
			got := noticeFrom(c.fields)
			if (got == nil) != (c.want == nil) {
				t.Fatalf("noticeFrom = %+v, want %+v", got, c.want)
			}
			if got == nil {
				return
			}
			if got.Kind != c.want.Kind || got.Agent != c.want.Agent || got.Subject != c.want.Subject || got.Cause != c.want.Cause ||
				got.Pod != c.want.Pod || got.Phase != c.want.Phase || got.Count != c.want.Count || got.Window != c.want.Window ||
				got.More != c.want.More || len(got.Dead) != len(c.want.Dead) {
				t.Fatalf("noticeFrom = %+v, want %+v", got, c.want)
			}
			for i := range got.Dead {
				if got.Dead[i] != c.want.Dead[i] {
					t.Fatalf("Dead[%d] = %+v, want %+v", i, got.Dead[i], c.want.Dead[i])
				}
			}
		})
	}
}

// A storm roll-up names at most noticeDeadMax agents and counts the rest.
func TestNoticeFromCapsNamedDeaths(t *testing.T) {
	var b strings.Builder
	b.WriteString("[")
	for i := 0; i < noticeDeadMax+5; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"id":"gd-a","name":"crew"}`)
	}
	b.WriteString("]")
	n := noticeFrom(map[string]string{"notice": "death_storm", "notice_count": "30", "notice_dead": b.String()})
	if n == nil || len(n.Dead) != noticeDeadMax || n.More != 30-noticeDeadMax || len(n.Rest) != 5 || n.Unnamed != 30-noticeDeadMax-5 {
		t.Fatalf("noticeFrom = %+v, want %d named, %d more (5 named behind it)", n, noticeDeadMax, 30-noticeDeadMax)
	}
}

// The count past the named agents opens on the rest of them (gd-HEky8ZLUVK),
// and says how many the report did not name; a count with no names behind it
// says so, with nothing to open.
func TestStormNoticeOpensOnTheRest(t *testing.T) {
	var dead []string
	for i := range noticeDeadMax + 2 {
		dead = append(dead, fmt.Sprintf(`{"id":"gd-a%d","name":"crew-%d"}`, i, i))
	}
	n := noticeFrom(map[string]string{"notice": "death_storm", "notice_count": "13", "notice_dead": "[" + strings.Join(dead, ",") + "]"})
	var b strings.Builder
	if err := fragments.ExecuteTemplate(&b, "notice", row{M: Message{Notice: n}, Base: "/base"}); err != nil {
		t.Fatal(err)
	}
	mustContain(t, b.String(), `<details class="cm-notice-rest"><summary class="cm-notice-more">and 5 more</summary>`,
		`<a href="/base/garden?bead=gd-a9">crew-9</a>`, `<li class="cm-notice-more">and 3 not named</li>`)
	if strings.Contains(b.String(), `<li class="cm-notice-more">and 5 more</li>`) {
		t.Errorf("the count is both a summary and a line:\n%s", b.String())
	}
	b.Reset()
	n = noticeFrom(map[string]string{"notice": "death_storm", "notice_count": "3", "notice_dead": `[{"id":"gd-a1","name":"crew-1"}]`})
	if err := fragments.ExecuteTemplate(&b, "notice", row{M: Message{Notice: n}, Base: "/base"}); err != nil {
		t.Fatal(err)
	}
	mustContain(t, b.String(), `<li class="cm-notice-more">and 2 not named</li>`)
	if strings.Contains(b.String(), "<details") {
		t.Errorf("a count with no names behind it opens on nothing:\n%s", b.String())
	}
}

// An agent closed by an involuntary death shows the died face with its cause,
// not the close reason's code as a tldr; a clean exit stays done.
func TestAgentCardDiedFace(t *testing.T) {
	agent := func(fields map[string]string) *beadsapi.BeadDetail {
		f := map[string]string{"agent": "crew-1", "project": "cl", "agent_state": "done"}
		for k, v := range fields {
			f[k] = v
		}
		return &beadsapi.BeadDetail{ID: "cl-a1", Type: "agent", Status: "closed", Labels: []string{"project:cl"}, UpdatedAt: time.Now(), Fields: f}
	}
	c, ok := agentCardFrom(agent(map[string]string{"close_reason": "context_cancelled_involuntary"}), "cl")
	if !ok || c.State != "died" || c.Label() != "Error" || c.Cause == "" || c.TLDR != "" {
		t.Fatalf("involuntary death: %+v", c)
	}
	c, _ = agentCardFrom(agent(map[string]string{"close_reason": "context_cancelled_involuntary",
		"controller_restart_reason": "account rotation deleted the pod"}), "cl")
	if !strings.HasPrefix(c.Cause, "account rotation deleted the pod — ") {
		t.Fatalf("controller reason not first: %q", c.Cause)
	}
	c, _ = agentCardFrom(agent(map[string]string{"close_reason": "context_cancelled_involuntary",
		"wrapup": `{"tldr":"half done"}`}), "cl")
	if c.State != "died" || c.TLDR != "half done" {
		t.Fatalf("a died agent's own wrap-up stays: %+v", c)
	}
	c, _ = agentCardFrom(agent(map[string]string{"close_reason": "completed"}), "cl")
	if c.State != "done" || c.Cause != "" || c.TLDR != "completed" {
		t.Fatalf("clean exit: %+v", c)
	}
}
