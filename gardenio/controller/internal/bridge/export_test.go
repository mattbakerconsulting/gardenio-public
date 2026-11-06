package bridge

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"

	"gardenio/controller/internal/agentwake"
	"gardenio/controller/internal/beadsapi"
)

// EvaluateTriggerBeadForTest runs one trigger-engine pass over bead, a trigger
// bead as the daemon stores it, with one match pending. It reports whether the
// engine closed the bead and the gardener spec it last wrote back (nil when it
// wrote none). External tests use it to pin how the engine treats a trigger
// another package builds (steward_pin_test.go).
func EvaluateTriggerBeadForTest(t *testing.T, bead *beadsapi.BeadDetail) (closed bool, written *GardenerSpec) {
	t.Helper()
	daemon := &mockTriggerDaemon{beads: []*beadsapi.BeadDetail{bead}}
	engine := NewTriggerEngine(TriggerEngineConfig{Daemon: daemon, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	trig := ParseTriggerDef(bead, "mock")
	trig.LastChecked = pastLastChecked()
	engine.RegisterSource(&mockTriggerSource{
		name:     "mock",
		triggers: []TriggerDef{trig},
		matches:  map[string][]TriggerMatch{bead.ID: {{Key: "M1", Title: "match one"}}},
	})
	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	closed = slices.Contains(daemon.closedIDs, bead.ID)
	for i := len(daemon.updatedFields) - 1; i >= 0; i-- {
		raw, ok := daemon.updatedFields[i]["gardener"]
		if daemon.updatedIDs[i] != bead.ID || !ok {
			continue
		}
		var g GardenerSpec
		if err := json.Unmarshal([]byte(raw), &g); err != nil {
			t.Fatalf("gardener write %s: %v", raw, err)
		}
		return closed, &g
	}
	return closed, nil
}

// SkippedBySchedulesOffForTest reports whether the engine skips trig, loaded
// by the source named sourceName, in a cycle where trig's project has
// schedules_enabled=false (`/schedule off <project>`) and triggers enabled.
// External tests use it to pin which triggers another package builds are
// schedule-gated (steward_pin_test.go).
func SkippedBySchedulesOffForTest(sourceName string, trig TriggerDef) bool {
	g := triggerGateState{projectSchedulesDisabled: map[string]bool{trig.Project: true}}
	return g.skipTrigger(scheduleSourceNames[sourceName], trig)
}

// viaCommons is a Commons message's text as the Slack mirror posts it as the
// bot: the author's name, then the words. Both are escaped, so a Commons
// message can never ping @channel or forge a link in Slack.
func viaCommons(name, text string) string {
	return viaCommonsWithFiles(name, text, nil)
}

// BeadUnbackedGardenerProse applies UnbackedGardenerProse to a raw trigger bead,
// reading the prompt, gardener block, and conditions pre-flight off its fields.
func BeadUnbackedGardenerProse(bead *beadsapi.BeadDetail) (markers []string, unbacked bool) {
	if bead == nil {
		return nil, false
	}
	hasGardener := strings.TrimSpace(bead.Fields["gardener"]) != ""
	hasGate := strings.TrimSpace(bead.Fields["conditions"]) != ""
	return UnbackedGardenerProse(bead.Fields["prompt"], hasGardener, hasGate)
}

// GitLabWebhookHandler returns an http.Handler that processes GitLab webhook
// events for merge request merges.
func GitLabWebhookHandler(gitlab *GitLabClient, daemon GitLabBeadClient, webhookSecret string, logger *slog.Logger) http.Handler {
	return GitLabWebhookHandlerWithConfig(GitLabWebhookConfig{
		GitLab:        gitlab,
		Daemon:        daemon,
		WebhookSecret: webhookSecret,
		Logger:        logger,
	})
}

// BeadUnreachableRepoPaths applies UnreachableRepoPaths to a raw trigger bead,
// reading the prompt, project, and action off its fields.
func BeadUnreachableRepoPaths(bead *beadsapi.BeadDetail) (paths []string, unreachable bool) {
	if bead == nil {
		return nil, false
	}
	return UnreachableRepoPaths(bead.Fields["prompt"], bead.Fields["project"], bead.Fields["action"])
}

// rewriteFormBody parses an application/x-www-form-urlencoded Slack payload,
// linkifies refs in the `text`, `blocks`, and `attachments` fields, and
// returns the re-encoded body. The bool is false when nothing changed.
func rewriteFormBody(body []byte, rules []linkRule) (string, bool) {
	vals, err := url.ParseQuery(string(body))
	if err != nil {
		return "", false
	}
	if !rewriteFormValues(vals, rules) {
		return "", false
	}
	return vals.Encode(), true
}

// matchConditions checks if all conditions match the bead event. This
// in-process form does not support daemon-backed operators (in_subgraph_of).
// Callers that may receive such operators should use
// BeadStateTriggerSource.matchConditionsCtx instead.
func matchConditions(conditions []beadCondition, bead *BeadEvent) bool {
	for _, c := range conditions {
		if !matchOneCondition(c, bead) {
			return false
		}
	}
	return true
}

// NewGitLabTriggerSource creates a GitLab trigger source.
func NewGitLabTriggerSource(gitlab *GitLabClient) *GitLabTriggerSource {
	return &GitLabTriggerSource{gitlab: gitlab, logger: slog.Default()}
}

// expandIssueTemplate expands tmpl with issue's template variables.
func expandIssueTemplate(tmpl string, issue JiraIssue) string {
	m := jiraIssueToMatch(issue)
	return ExpandTemplate(tmpl, m.TemplateVars)
}

// sanitizeTriggerAgentName is triggerAgentName for a trigger with only a
// project set.
func sanitizeTriggerAgentName(project, matchKey string) string {
	return triggerAgentName(TriggerDef{Project: project}, matchKey)
}

// GrafanaBridgeConfigKeys returns the keys EnsureGrafanaBridgeConfigs seeds.
func GrafanaBridgeConfigKeys() []string {
	return append([]string(nil), grafanaBridgeConfigKeys...)
}

// memorySlackLinkStore is an in-memory SlackLinkStore for tests.
type memorySlackLinkStore struct {
	mu    sync.Mutex
	links map[string]*SlackLink
}

func newMemorySlackLinkStore() *memorySlackLinkStore {
	return &memorySlackLinkStore{links: map[string]*SlackLink{}}
}

func (m *memorySlackLinkStore) Get(id string) (*SlackLink, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.links[id]
	if !ok {
		return nil, false, nil
	}
	cp := *l
	return &cp, true, nil
}

func (m *memorySlackLinkStore) Put(l *SlackLink) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *l
	m.links[l.SlackUserID] = &cp
	return nil
}

func (m *memorySlackLinkStore) List() ([]*SlackLink, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*SlackLink, 0, len(m.links))
	for _, l := range m.links {
		cp := *l
		out = append(out, &cp)
	}
	return out, nil
}

func (m *memorySlackLinkStore) Delete(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.links, id)
	return nil
}

// killAgent closes an agent bead and removes its Slack card.
// It is called by both the /kill slash command and the Clear button handler.
// If force is false, it first sends the agent a control:kill mail and waits
// up to killWaitMax for it to stop; if the mail cannot be sent or the agent
// does not stop in time, it falls back to an immediate hard-close.
func (b *Bot) killAgent(ctx context.Context, agentName string, force bool) error {
	return b.killAgentBead(ctx, agentName, "", force)
}

// ResolveAndNudge finds the agent assigned to the given JIRA ticket's task bead
// and delivers a nudge with the mention context. If the agent is dead (no active
// agent bead, or one closed, terminal or stopping), it spawns a new agent
// pre-assigned to the task.
func (r *JiraAgentResolver) ResolveAndNudge(ctx context.Context, agentName, jiraKey, project, taskID, prompt string) error {
	return r.resolveAndNudge(ctx, agentName, jiraKey, project, taskID, "", prompt, false)
}

// IsTracked returns true if the JIRA key is already tracked.
func (p *JiraPoller) IsTracked(key string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.tracked[key]
	return ok
}

// TrackedCount returns the number of tracked JIRA issues.
func (p *JiraPoller) TrackedCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.tracked)
}

// CatchUpTrigger populates the seen set for a single trigger.
func (e *JiraTriggerEvaluator) CatchUpTrigger(ctx context.Context, triggerID string) {
	e.engine.CatchUpTrigger(ctx, triggerID)
}

// ScanOnce performs exactly one pass and returns its stats, for tests/probes
// that must trigger the bot deterministically rather than wait out the interval.
func (r *MRSchemaFlagReconciler) ScanOnce(ctx context.Context) schemaFlagStats {
	return r.scan(ctx)
}

// AllowedProjects returns the sorted allow-list (for startup logging). FSM is
// appended when the toggle is on, reflecting the effective writable set.
func (g *ProjectSyncGate) AllowedProjects() []string {
	if g == nil {
		return nil
	}
	out := make([]string, 0, len(g.allowed)+1)
	for p := range g.allowed {
		if g.blocked[p] && p != fsmProjectKey {
			continue
		}
		out = append(out, p)
	}
	if g.fsmSyncEnabled {
		out = append(out, fsmProjectKey)
	}
	sort.Strings(out)
	return dedupeSorted(out)
}

// dedupeSorted removes adjacent duplicates from a sorted slice in place.
func dedupeSorted(in []string) []string {
	if len(in) < 2 {
		return in
	}
	out := in[:1]
	for _, s := range in[1:] {
		if s != out[len(out)-1] {
			out = append(out, s)
		}
	}
	return out
}

// eventWakeDormantAgent is agentwake.EventWakeDormant.
func eventWakeDormantAgent(ctx context.Context, daemon eventWakeClient, logger *slog.Logger, bead *beadsapi.BeadDetail, reason, actor string) (bool, error) {
	return agentwake.EventWakeDormant(ctx, daemon, logger, bead, reason, actor)
}
