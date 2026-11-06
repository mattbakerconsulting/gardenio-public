package bridge

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/slack-go/slack"

	"gardenio/controller/internal/beadsapi"
)

// localCardText flattens section text, section fields, header and context
// text so the runner card's field grid is searchable too.
func localCardText(blocks []slack.Block) string {
	var sb strings.Builder
	for _, blk := range blocks {
		switch b := blk.(type) {
		case *slack.SectionBlock:
			if b.Text != nil {
				sb.WriteString(b.Text.Text + "\n")
			}
			for _, f := range b.Fields {
				sb.WriteString(f.Text + "\n")
			}
		case *slack.HeaderBlock:
			sb.WriteString(b.Text.Text + "\n")
		case *slack.ContextBlock:
			for _, el := range b.ContextElements.Elements {
				if t, ok := el.(*slack.TextBlockObject); ok {
					sb.WriteString(t.Text + "\n")
				}
			}
		}
	}
	return sb.String()
}

func TestLocalBadge_HeaderAndRoster(t *testing.T) {
	fields := map[string]string{"execution_target": "laptop", "operated_by": "oidc:alice@example.com", "runtime_id": "gd-r1"}
	lb := localBadgeFor(fields, "laptop")
	if lb == nil {
		t.Fatal("a laptop agent must get a badge")
	}
	if got := lb.headerSuffix(Garden); !strings.Contains(got, "local, running as alice@example.com on laptop") || !strings.Contains(got, "🧑‍💻") {
		t.Fatalf("header suffix: %q", got)
	}
	if got := lb.headerSuffix(Business); !strings.Contains(got, "[LOCAL] local, running as") {
		t.Fatalf("business header suffix: %q", got)
	}
	// A pod agent, and an escalation executor pod (execution_target=local,
	// the fleet meaning), get no badge.
	for _, target := range []string{"", "k8s", "local"} {
		if localBadgeFor(map[string]string{"execution_target": target}, "laptop") != nil {
			t.Fatalf("execution_target=%q must get no badge", target)
		}
	}
	if lb := localBadgeFor(fields, ""); lb == nil || lb.Hostname != "an unregistered machine" {
		t.Fatalf("an unknown runner still badges, with a placeholder host: %+v", lb)
	}

	line := rosterLineSuffix(Garden, beadsapi.AgentBead{Metadata: fields}, map[string]string{"gd-r1": "laptop"})
	if !strings.Contains(line, "local (alice@example.com on laptop)") || !strings.Contains(line, "owner-billed") {
		t.Fatalf("roster suffix: %q", line)
	}
	if line := rosterLineSuffix(Garden, beadsapi.AgentBead{Metadata: fields}, nil); !strings.Contains(line, "unknown host") {
		t.Fatalf("roster suffix with no runner listing: %q", line)
	}
	if got := rosterLineSuffix(Garden, beadsapi.AgentBead{Metadata: map[string]string{"execution_target": "k8s"}}, nil); got != "" {
		t.Fatalf("pod agent roster suffix must be empty, got %q", got)
	}
}

func TestBuildAgentCardBlocks_LocalBadgeInHeader(t *testing.T) {
	blocks := buildAgentCardBlocks(Garden, "local-laptop", "", 0, "working", "", time.Now(), "", "", "local", "$1.23", "", nil, "v", false, triggerCardInfo{},
		&localBadge{Owner: "oidc:alice@example.com", Hostname: "laptop"})
	text := localCardText(blocks)
	if !strings.Contains(text, "running as alice@example.com on laptop") || !strings.Contains(text, "owner-billed") {
		t.Fatalf("card missing local badge: %s", text)
	}
	if strings.Contains(text, "$1.23") {
		t.Fatalf("a laptop agent's card must not show a fleet cost: %s", text)
	}
	if strings.Contains(text, "· local ·") {
		t.Fatalf("the default local role must not repeat beside the badge: %s", text)
	}

	pod := localCardText(buildAgentCardBlocks(Garden, "crew-bot", "", 0, "working", "", time.Now(), "", "", "crew", "$1.23", "", nil, "v", false, triggerCardInfo{}, nil))
	if strings.Contains(pod, "owner-billed") || strings.Contains(pod, "running as") || !strings.Contains(pod, "$1.23") {
		t.Fatalf("a pod agent's card must be unchanged: %s", pod)
	}
}

func TestFetchAgentRollup_CarriesLocalBadge(t *testing.T) {
	d := newMockDaemon()
	seedRunner(d, "gd-r1", "laptop", "online")
	a := laptopAgentBead("gd-a1", "local-laptop", "gd-r1")
	a.Fields["operated_by"] = "oidc:alice@example.com"
	d.beads["gd-a1"] = a
	b := newPlacementBot(d)

	r := b.fetchAgentRollup(context.Background(), Garden, "local-laptop")
	if r.Local == nil || r.Local.Hostname != "laptop" || r.Local.Owner != "oidc:alice@example.com" {
		t.Fatalf("rollup badge: %+v", r.Local)
	}
	// Suppressed cost reporting still badges: the badge is not a cost.
	b.notifConfig.SuppressCostReporting = true
	if r := b.fetchAgentRollup(context.Background(), Garden, "local-laptop"); r.Local == nil || r.Cost != "" {
		t.Fatalf("suppressed rollup: %+v", r)
	}
}

func TestRunnerHostnames_Cached(t *testing.T) {
	d := newMockDaemon()
	seedRunner(d, "gd-r1", "laptop", "online")
	b := newPlacementBot(d)
	if got := b.runnerHostnames(context.Background())["gd-r1"]; got != "laptop" {
		t.Fatalf("hostname: %q", got)
	}
	seedRunner(d, "gd-r2", "desk", "online")
	if _, ok := b.runnerHostnames(context.Background())["gd-r2"]; ok {
		t.Fatal("a second call within the TTL must reuse the cached listing")
	}
	b.runnerHosts.at = time.Now().Add(-2 * runnerHostnameTTL)
	if got := b.runnerHostnames(context.Background())["gd-r2"]; got != "desk" {
		t.Fatalf("an expired cache must relist, got %q", got)
	}
}

func TestRosterCommand_LocalAgentLine(t *testing.T) {
	d := newMockDaemon()
	seedRunner(d, "gd-r1", "laptop", "online")
	a := laptopAgentBead("gd-a1", "local-laptop", "gd-r1")
	a.Fields["operated_by"] = "oidc:alice@example.com"
	a.Fields["role"] = "local"
	a.Fields["total_cost_usd"] = "4.56"
	d.beads["gd-a1"] = a
	var captured slackAPICalls
	srv := newCapturingSlackServer(t, &captured)
	defer srv.Close()
	b := newTestBot(d, srv)

	b.handleRosterCommand(context.Background(), slack.SlashCommand{Command: "/roster"})

	eph, _ := url.QueryUnescape(captured.bodyForPath("/chat.postEphemeral"))
	for _, want := range []string{"local-laptop", "local (alice@example.com on laptop) · owner-billed"} {
		if !strings.Contains(eph, want) {
			t.Errorf("roster missing %q: %s", want, eph)
		}
	}
	if strings.Contains(eph, "4.56") {
		t.Errorf("a laptop agent's roster line must not show a cost: %s", eph)
	}
}
