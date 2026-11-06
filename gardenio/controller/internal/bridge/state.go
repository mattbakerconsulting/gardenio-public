// Package bridge provides state persistence for the slack-bridge.
//
// StateManager persists message references (decision messages, dashboard)
// to a JSON file so that Slack message threading and update-in-place
// survive pod restarts.
package bridge

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// MessageRef tracks a Slack message by channel and timestamp.
type MessageRef struct {
	ChannelID string `json:"channel_id"`
	Timestamp string `json:"timestamp"`
	Agent     string `json:"agent,omitempty"`     // agent identity (for decision messages)
	ThreadTS  string `json:"thread_ts,omitempty"` // parent thread ts (set when message is a thread reply)
	// Compressed records that pruneStaleAgentCards has already rewritten this
	// card into its terminal/compact form. Agent cards are deliberately kept in
	// persisted state forever so findAgentCardByThread can still map a thread
	// back to its agent (see CompactStaleEntries) — but startup rehydrates them
	// all into the hot cache, so without this marker every restart re-compressed
	// the entire historical backlog, burning the shared Slack chat.update budget
	// for over an hour and starving live cards (ka-0000008156).
	Compressed bool `json:"compressed,omitempty"`
}

// ThreadReplyDLQEntry is one human thread reply that failed to persist as a
// conversation-bubble bead and is queued for later replay (ka-0000000612). It
// captures exactly the inputs recordUnifiedThreadReply needs to re-attempt the
// write; the drain loop rebuilds the unifiedThreadReplyInput from these fields.
// Attachment metadata is intentionally NOT captured — a replayed reply re-fetches
// files from Slack at drain time, and a caption-less attachment-only reply is
// still enqueued (its Text carries the synthesized placeholder so the body is
// never blank).
type ThreadReplyDLQEntry struct {
	IdempotencyKey string    `json:"idempotency_key"`
	Channel        string    `json:"channel"`
	ThreadTS       string    `json:"thread_ts"`
	MessageTS      string    `json:"message_ts"`
	Text           string    `json:"text"`
	SlackUser      string    `json:"slack_user"`
	Project        string    `json:"project"`
	AgentName      string    `json:"agent_name"`
	ExtraLabel     string    `json:"extra_label"`
	Head           string    `json:"head,omitempty"` // the thread's wish head, when it was resolved
	EnqueuedAt     time.Time `json:"enqueued_at"`
	Attempts       int       `json:"attempts"`
}

// JiraInboundCommentDLQEntry is one inbound Jira comment mirror whose write onto
// the ticket's task bead failed terminally (after the bounded transient retry)
// and is queued for later replay (epic ka-0000007401 — Slack inbound-durability
// parity with ThreadReplyDLQEntry). It captures exactly the inputs the drain loop
// needs to re-attempt the mirror: the target bead, the fully-formatted comment
// body, and the reply-anchor fields. Keyed by (bead, jira comment id) so a
// re-enqueue of the same comment coalesces to one entry.
//
// Unlike the Slack path, the kbeads comment-create endpoint has no idempotency
// key, so a lost-response replay cannot be made a daemon-side no-op. The drain
// loop instead checks GetComments for a marker unique to this comment before
// re-posting, so a comment that actually landed on the first (lost-response)
// attempt is not double-posted. That marker is CommentText itself — the mirror
// body embeds the Jira author + text and is stable across replays.
type JiraInboundCommentDLQEntry struct {
	Key            string    `json:"key"` // dedup/replay key: "{beadID}:{jiraCommentID}"
	BeadID         string    `json:"bead_id"`
	JiraKey        string    `json:"jira_key"`
	JiraComment    string    `json:"jira_comment_id,omitempty"` // originating comment id → jira_comment_id reply anchor
	CommentText    string    `json:"comment_text"`              // fully-formatted mirror body (also the replay existence marker)
	SetHasComments bool      `json:"set_has_comments"`          // whether to (re)set jira_has_comments=true
	EnqueuedAt     time.Time `json:"enqueued_at"`
	Attempts       int       `json:"attempts"`
}

// DashboardRef tracks the persistent dashboard message.
type DashboardRef struct {
	ChannelID string `json:"channel_id"`
	Timestamp string `json:"timestamp"`
	LastHash  string `json:"last_hash,omitempty"` // content hash for change detection
}

// ThreadAgentEntry tracks a thread-agent binding with an activity timestamp
// for TTL-based expiry.
type ThreadAgentEntry struct {
	Agent      string    `json:"agent"`
	LastActive time.Time `json:"last_active"`
}

// UnmarshalJSON handles backward compatibility: old state files store
// thread_agents values as plain strings, new ones as objects.
func (e *ThreadAgentEntry) UnmarshalJSON(data []byte) error {
	// Try plain string first (backward compat with old state files).
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		e.Agent = s
		e.LastActive = time.Time{}
		return nil
	}
	// Object form.
	type entry ThreadAgentEntry // avoid infinite recursion
	var obj entry
	if err := json.Unmarshal(data, &obj); err != nil {
		return err
	}
	*e = ThreadAgentEntry(obj)
	return nil
}

// StateData is the JSON-serialized state structure.
type StateData struct {
	DecisionMessages map[string]MessageRef `json:"decision_messages,omitempty"` // bead ID → message ref
	// DecisionMessageMirrors holds ADDITIONAL messages rendering the same
	// decision beyond the primary in DecisionMessages — see
	// decision_message_mirrors.go. omitempty keeps it absent from state.json
	// until a decision actually has mirrors, so this is a no-op on disk today.
	DecisionMessageMirrors map[string][]MessageRef     `json:"decision_message_mirrors,omitempty"` // bead ID → mirror refs
	ChatMessages           map[string]MessageRef       `json:"chat_messages,omitempty"`            // bead ID → message ref (chat forwarding)
	AgentCards             map[string]MessageRef       `json:"agent_cards,omitempty"`              // agent identity → status card message ref
	ThreadAgents           map[string]ThreadAgentEntry `json:"thread_agents,omitempty"`            // head id (or, before the head is known, "{channel}:{thread_ts}") → agent entry
	ThreadHeads            map[string]string           `json:"thread_heads,omitempty"`             // "{channel}:{thread_ts}" → head id: a Slack thread's alias of its head
	ListenThreads          map[string]bool             `json:"listen_threads,omitempty"`           // "{channel}:{thread_ts}" → true if --listen mode
	NudgeThrottles         map[string]time.Time        `json:"nudge_throttles,omitempty"`          // "agent:thread_ts" → last nudge time
	Dashboard              *DashboardRef               `json:"dashboard,omitempty"`
	LastEventID            string                      `json:"last_event_id,omitempty"` // SSE event ID for reconnection
	// LastReconcileAt is the high-water mark consumed by the trigger:bead
	// reconciler (ka-0000004769). On each reconcile tick the bridge queries
	// the daemon for beads with updated_at > LastReconcileAt and synthesises
	// "reconciled" events into the bead-state trigger pipeline. Survives
	// bridge crashes and postgres failovers because the watermark lives in
	// the state file, not the daemon's in-memory SSE ring buffer.
	LastReconcileAt time.Time `json:"last_reconcile_at,omitempty"`
	// ThreadReplyDLQ holds human thread replies whose conversation-bubble
	// write to the daemon failed terminally (ka-0000000612). A background
	// drain loop replays them so a transient daemon outage / restart no
	// longer silently drops a reply from the /threads conversation lane. It
	// lives in the state file so a queued reply survives a bridge crash. Keyed
	// by the reply's idempotency key so a re-enqueue of the same reply
	// coalesces to one entry and replay is exactly-once at the daemon.
	ThreadReplyDLQ map[string]ThreadReplyDLQEntry `json:"thread_reply_dlq,omitempty"`
	// JiraInboundCommentDLQ holds inbound Jira comment mirrors whose write onto
	// the ticket's task bead failed terminally (epic ka-0000007401). A background
	// drain loop in the jira-bridge replays them so a transient daemon outage /
	// restart no longer silently drops a human follow-up from the bead
	// conversation. Same durability contract as ThreadReplyDLQ: lives in the state
	// file so a queued mirror survives a bridge crash; keyed by (bead, jira
	// comment id) so a re-enqueue coalesces to one entry.
	JiraInboundCommentDLQ map[string]JiraInboundCommentDLQEntry `json:"jira_inbound_comment_dlq,omitempty"`
}

// StateManager provides thread-safe persistence of Slack message references.
type StateManager struct {
	mu   sync.RWMutex
	path string
	data StateData
}

// NewStateManager creates a state manager that persists to the given path.
// If the file exists, its contents are loaded.
func NewStateManager(path string) (*StateManager, error) {
	sm := &StateManager{
		path: path,
		data: StateData{
			DecisionMessages:       make(map[string]MessageRef),
			DecisionMessageMirrors: make(map[string][]MessageRef),
			ChatMessages:           make(map[string]MessageRef),
			AgentCards:             make(map[string]MessageRef),
			ThreadAgents:           make(map[string]ThreadAgentEntry),
			ThreadHeads:            make(map[string]string),
			ListenThreads:          make(map[string]bool),
			NudgeThrottles:         make(map[string]time.Time),
			ThreadReplyDLQ:         make(map[string]ThreadReplyDLQEntry),
			JiraInboundCommentDLQ:  make(map[string]JiraInboundCommentDLQEntry),
		},
	}
	if err := sm.load(); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("load state: %w", err)
	}
	return sm, nil
}

// --- Decision Messages ---

// GetDecisionMessage returns the message ref for a decision bead.
func (sm *StateManager) GetDecisionMessage(beadID string) (MessageRef, bool) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	ref, ok := sm.data.DecisionMessages[beadID]
	return ref, ok
}

// SetDecisionMessage stores a message ref for a decision bead and persists.
func (sm *StateManager) SetDecisionMessage(beadID string, ref MessageRef) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.data.DecisionMessages[beadID] = ref
	return sm.saveLocked()
}

// RemoveDecisionMessage removes a message ref for a decision bead and persists.
func (sm *StateManager) RemoveDecisionMessage(beadID string) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	delete(sm.data.DecisionMessages, beadID)
	return sm.saveLocked()
}

// --- Decision Message Mirrors ---
//
// Mirrors are secondary Slack renderings of a decision (see
// decision_message_mirrors.go). They persist alongside the primary so a bridge
// restart does not orphan a copy that then goes permanently stale.

// GetDecisionMessageMirrors returns the mirror refs for a decision.
func (sm *StateManager) GetDecisionMessageMirrors(beadID string) []MessageRef {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	refs := sm.data.DecisionMessageMirrors[beadID]
	out := make([]MessageRef, len(refs))
	copy(out, refs)
	return out
}

// SetDecisionMessageMirrors replaces the mirror refs for a decision and
// persists. An empty set removes the entry rather than storing a null.
func (sm *StateManager) SetDecisionMessageMirrors(beadID string, refs []MessageRef) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if sm.data.DecisionMessageMirrors == nil {
		sm.data.DecisionMessageMirrors = make(map[string][]MessageRef)
	}
	if len(refs) == 0 {
		delete(sm.data.DecisionMessageMirrors, beadID)
	} else {
		sm.data.DecisionMessageMirrors[beadID] = refs
	}
	return sm.saveLocked()
}

// RemoveDecisionMessageMirrors drops all mirror refs for a decision and persists.
func (sm *StateManager) RemoveDecisionMessageMirrors(beadID string) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	delete(sm.data.DecisionMessageMirrors, beadID)
	return sm.saveLocked()
}

// AllDecisionMessageMirrors returns a copy of all tracked decision mirrors.
func (sm *StateManager) AllDecisionMessageMirrors() map[string][]MessageRef {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	out := make(map[string][]MessageRef, len(sm.data.DecisionMessageMirrors))
	for k, v := range sm.data.DecisionMessageMirrors {
		refs := make([]MessageRef, len(v))
		copy(refs, v)
		out[k] = refs
	}
	return out
}

// AllDecisionMessages returns a copy of all tracked decision messages.
func (sm *StateManager) AllDecisionMessages() map[string]MessageRef {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	out := make(map[string]MessageRef, len(sm.data.DecisionMessages))
	for k, v := range sm.data.DecisionMessages {
		out[k] = v
	}
	return out
}

// --- Chat Messages ---

// GetChatMessage returns the message ref for a chat bead.
func (sm *StateManager) GetChatMessage(beadID string) (MessageRef, bool) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	ref, ok := sm.data.ChatMessages[beadID]
	return ref, ok
}

// SetChatMessage stores a message ref for a chat bead and persists.
func (sm *StateManager) SetChatMessage(beadID string, ref MessageRef) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.data.ChatMessages[beadID] = ref
	return sm.saveLocked()
}

// RemoveChatMessage removes a message ref for a chat bead and persists.
func (sm *StateManager) RemoveChatMessage(beadID string) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	delete(sm.data.ChatMessages, beadID)
	return sm.saveLocked()
}

// AllChatMessages returns a copy of all tracked chat messages.
func (sm *StateManager) AllChatMessages() map[string]MessageRef {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	out := make(map[string]MessageRef, len(sm.data.ChatMessages))
	for k, v := range sm.data.ChatMessages {
		out[k] = v
	}
	return out
}

// --- Agent Cards ---

// GetAgentCard returns the status card message ref for an agent.
func (sm *StateManager) GetAgentCard(agent string) (MessageRef, bool) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	ref, ok := sm.data.AgentCards[agent]
	return ref, ok
}

// SetAgentCard stores a status card message ref for an agent and persists.
func (sm *StateManager) SetAgentCard(agent string, ref MessageRef) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.data.AgentCards[agent] = ref
	return sm.saveLocked()
}

// MarkAgentCardCompressed flags the persisted card for agent as already
// compressed, so a later restart does not rehydrate it and re-compress it
// (ka-0000008156).
//
// Matching is by MESSAGE REF (channel + timestamp) rather than by map key. The
// prune loop iterates the hot cache, whose keys hydration normalizes to short
// names, while persisted state may still hold a legacy full-path key for the
// same Slack message. Keying off the ref marks whichever entry actually owns
// that message instead of inserting a second, short-named duplicate that would
// leave the original unmarked and looping forever. Falls back to the supplied
// key when no ref matches, so a card missing from state is still recorded.
func (sm *StateManager) MarkAgentCardCompressed(agent string, ref MessageRef) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	for key, existing := range sm.data.AgentCards {
		if existing.ChannelID == ref.ChannelID && existing.Timestamp == ref.Timestamp {
			if existing.Compressed {
				return nil // already marked; avoid a redundant fsync
			}
			existing.Compressed = true
			sm.data.AgentCards[key] = existing
			return sm.saveLocked()
		}
	}
	ref.Compressed = true
	sm.data.AgentCards[agent] = ref
	return sm.saveLocked()
}

// RemoveAgentCard removes a status card message ref for an agent and persists.
func (sm *StateManager) RemoveAgentCard(agent string) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	delete(sm.data.AgentCards, agent)
	return sm.saveLocked()
}

// AllAgentCards returns a copy of all tracked agent status card messages.
func (sm *StateManager) AllAgentCards() map[string]MessageRef {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	out := make(map[string]MessageRef, len(sm.data.AgentCards))
	for k, v := range sm.data.AgentCards {
		out[k] = v
	}
	return out
}

// --- Thread Agents ---

// Thread bindings (design spec 2026-10-19-lands-commons-design.md, section
// 2.3, slice 4d). ThreadAgents is a cache of "which agent works this thread",
// keyed by the thread's wish head bead id. A Slack thread is an alias of its
// head (ThreadHeads, "{channel}:{thread_ts}" → head id), so every call keyed on
// the Slack tuple reaches the same entry, and a Commons thread with no Slack
// copy is bound by its head alone. An entry written before the head was known
// (an older state file, a spawn whose head mint failed) stays keyed on the
// tuple and is read as before; BindThreadHead moves it under the head. A head
// id never contains ':', a tuple key always does.

// threadAgentKey builds the Slack tuple key of a thread.
func threadAgentKey(channel, threadTS string) string {
	return channel + ":" + threadTS
}

// isHeadKey reports whether a ThreadAgents key is a head id, not a tuple.
func isHeadKey(key string) bool { return key != "" && !strings.Contains(key, ":") }

// bindingKeyLocked is the ThreadAgents key of a Slack thread: its head's id
// when the thread is aliased to one, else the tuple. Caller holds sm.mu.
func (sm *StateManager) bindingKeyLocked(channel, threadTS string) string {
	tuple := threadAgentKey(channel, threadTS)
	if head := sm.data.ThreadHeads[tuple]; head != "" {
		return head
	}
	return tuple
}

// GetThreadAgent returns the agent identity bound to a Slack thread.
func (sm *StateManager) GetThreadAgent(channel, threadTS string) (string, bool) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	entry, ok := sm.data.ThreadAgents[sm.bindingKeyLocked(channel, threadTS)]
	return entry.Agent, ok
}

// SetThreadAgent stores a thread→agent association with the current timestamp
// and persists.
func (sm *StateManager) SetThreadAgent(channel, threadTS, agent string) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.data.ThreadAgents[sm.bindingKeyLocked(channel, threadTS)] = ThreadAgentEntry{
		Agent:      agent,
		LastActive: time.Now(),
	}
	return sm.saveLocked()
}

// TouchThreadAgent refreshes the activity timestamp for an existing
// thread→agent binding without changing the agent. No-op if the key
// does not exist. Persists on update.
func (sm *StateManager) TouchThreadAgent(channel, threadTS string) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	return sm.touchLocked(sm.bindingKeyLocked(channel, threadTS))
}

func (sm *StateManager) touchLocked(key string) error {
	entry, ok := sm.data.ThreadAgents[key]
	if !ok {
		return nil
	}
	entry.LastActive = time.Now()
	sm.data.ThreadAgents[key] = entry
	return sm.saveLocked()
}

// RemoveThreadAgent removes a thread→agent association and persists. The
// thread stays aliased to its head: the head is still the thread's.
func (sm *StateManager) RemoveThreadAgent(channel, threadTS string) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	delete(sm.data.ThreadAgents, sm.bindingKeyLocked(channel, threadTS))
	return sm.saveLocked()
}

// BindThreadHead aliases a Slack thread to its wish head, so the thread's
// binding is keyed on the head from now on. An entry still keyed on the tuple
// moves under the head, unless the head has one of its own already. A no-op
// for an empty head or an invalid tuple.
func (sm *StateManager) BindThreadHead(channel, threadTS, head string) error {
	if !isHeadKey(head) || channel == "" || threadTS == "" {
		return nil
	}
	sm.mu.Lock()
	defer sm.mu.Unlock()
	tuple := threadAgentKey(channel, threadTS)
	if sm.data.ThreadHeads[tuple] == head {
		return nil
	}
	sm.data.ThreadHeads[tuple] = head
	if entry, ok := sm.data.ThreadAgents[tuple]; ok {
		if _, taken := sm.data.ThreadAgents[head]; !taken {
			sm.data.ThreadAgents[head] = entry
		}
		delete(sm.data.ThreadAgents, tuple)
	}
	return sm.saveLocked()
}

// ThreadHead returns the wish head a Slack thread is aliased to.
func (sm *StateManager) ThreadHead(channel, threadTS string) (string, bool) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	head := sm.data.ThreadHeads[threadAgentKey(channel, threadTS)]
	return head, head != ""
}

// SlackThreadOfHead returns the Slack thread aliased to head, if any.
func (sm *StateManager) SlackThreadOfHead(head string) (channel, threadTS string, ok bool) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.slackThreadOfHeadLocked(head)
}

func (sm *StateManager) slackThreadOfHeadLocked(head string) (channel, threadTS string, ok bool) {
	if head == "" {
		return "", "", false
	}
	for tuple, h := range sm.data.ThreadHeads {
		if h == head {
			channel, threadTS = parseThreadAgentKey(tuple)
			return channel, threadTS, channel != ""
		}
	}
	return "", "", false
}

// GetThreadAgentByHead returns the agent bound to the thread whose head is head.
func (sm *StateManager) GetThreadAgentByHead(head string) (string, bool) {
	if !isHeadKey(head) {
		return "", false
	}
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	entry, ok := sm.data.ThreadAgents[head]
	return entry.Agent, ok
}

// SetThreadAgentByHead binds agent to the thread whose head is head and
// persists.
func (sm *StateManager) SetThreadAgentByHead(head, agent string) error {
	if !isHeadKey(head) {
		return fmt.Errorf("thread binding: %q is no head id", head)
	}
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.data.ThreadAgents[head] = ThreadAgentEntry{Agent: agent, LastActive: time.Now()}
	return sm.saveLocked()
}

// TouchThreadAgentByHead refreshes the binding of head's thread, if any.
func (sm *StateManager) TouchThreadAgentByHead(head string) error {
	if !isHeadKey(head) {
		return nil
	}
	sm.mu.Lock()
	defer sm.mu.Unlock()
	return sm.touchLocked(head)
}

// RemoveThreadAgentByHead unbinds head's thread and persists.
func (sm *StateManager) RemoveThreadAgentByHead(head string) error {
	if !isHeadKey(head) {
		return nil
	}
	sm.mu.Lock()
	defer sm.mu.Unlock()
	delete(sm.data.ThreadAgents, head)
	return sm.saveLocked()
}

// RemoveThreadAgentByAgent removes all thread associations for a given agent
// and their corresponding listen-thread flags, then persists.
func (sm *StateManager) RemoveThreadAgentByAgent(agent string) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	for k, entry := range sm.data.ThreadAgents {
		if entry.Agent == agent {
			sm.dropBindingLocked(k)
		}
	}
	return sm.saveLocked()
}

// dropBindingLocked deletes the binding under key with its listen flag (keyed
// on the Slack tuple). A head keeps its Slack alias. Caller holds sm.mu.
func (sm *StateManager) dropBindingLocked(key string) {
	delete(sm.data.ThreadAgents, key)
	if !isHeadKey(key) {
		delete(sm.data.ListenThreads, key)
		return
	}
	if channel, ts, ok := sm.slackThreadOfHeadLocked(key); ok {
		delete(sm.data.ListenThreads, threadAgentKey(channel, ts))
	}
}

// ThreadBinding is one thread→agent binding: the thread's head, when known,
// and its Slack thread, when it has one.
type ThreadBinding struct {
	Head     string
	Channel  string
	ThreadTS string
	Agent    string
}

// AllThreadBindings returns a copy of every binding.
func (sm *StateManager) AllThreadBindings() []ThreadBinding {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	out := make([]ThreadBinding, 0, len(sm.data.ThreadAgents))
	for k, entry := range sm.data.ThreadAgents {
		b := ThreadBinding{Agent: entry.Agent}
		if isHeadKey(k) {
			b.Head = k
			b.Channel, b.ThreadTS, _ = sm.slackThreadOfHeadLocked(k)
		} else {
			b.Channel, b.ThreadTS = parseThreadAgentKey(k)
		}
		out = append(out, b)
	}
	return out
}

// AllThreadAgents returns a copy of all thread→agent mappings (agent names
// only), keyed "{channel}:{thread_ts}" for a thread with a Slack copy and by
// head id for one without.
func (sm *StateManager) AllThreadAgents() map[string]string {
	out := map[string]string{}
	for _, b := range sm.AllThreadBindings() {
		key := b.Head
		if b.Channel != "" {
			key = threadAgentKey(b.Channel, b.ThreadTS)
		}
		out[key] = b.Agent
	}
	return out
}

// ClearAllThreadAgents removes all thread→agent associations and persists.
func (sm *StateManager) ClearAllThreadAgents() (int, error) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	n := len(sm.data.ThreadAgents)
	sm.data.ThreadAgents = make(map[string]ThreadAgentEntry)
	return n, sm.saveLocked()
}

// GetThreadAgentsByChannel returns agent names for all threads in a given
// Slack channel.
func (sm *StateManager) GetThreadAgentsByChannel(channel string) []string {
	var agents []string
	for _, b := range sm.AllThreadBindings() {
		if b.Channel == channel {
			agents = append(agents, b.Agent)
		}
	}
	return agents
}

// --- Listen Threads ---

// IsListenThread returns whether a thread has --listen mode enabled.
func (sm *StateManager) IsListenThread(channel, threadTS string) bool {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.data.ListenThreads[threadAgentKey(channel, threadTS)]
}

// SetListenThread marks a thread as having --listen mode and persists.
func (sm *StateManager) SetListenThread(channel, threadTS string) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.data.ListenThreads[threadAgentKey(channel, threadTS)] = true
	return sm.saveLocked()
}

// RemoveListenThread removes the --listen flag for a thread and persists.
func (sm *StateManager) RemoveListenThread(channel, threadTS string) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	delete(sm.data.ListenThreads, threadAgentKey(channel, threadTS))
	return sm.saveLocked()
}

// --- Nudge Throttles ---

// CheckAndSetNudgeThrottle atomically checks whether a nudge for the given key
// is within the throttle interval. If throttled, returns true without modifying
// state. Otherwise, records the current time and persists.
// This survives bridge restarts, preventing duplicate bead creation on SSE replay.
func (sm *StateManager) CheckAndSetNudgeThrottle(key string, interval time.Duration) (bool, error) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if last, ok := sm.data.NudgeThrottles[key]; ok && time.Since(last) < interval {
		return true, nil // throttled
	}
	sm.data.NudgeThrottles[key] = time.Now()
	return false, sm.saveLocked()
}

// CleanExpiredNudgeThrottles removes throttle entries older than maxAge
// and persists. Safe for concurrent use.
func (sm *StateManager) CleanExpiredNudgeThrottles(maxAge time.Duration) (int, error) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	removed := sm.cleanExpiredNudgeThrottlesLocked(maxAge)
	if removed > 0 {
		return removed, sm.saveLocked()
	}
	return 0, nil
}

// cleanExpiredNudgeThrottlesLocked removes throttle entries older than maxAge.
// Caller must hold sm.mu.
func (sm *StateManager) cleanExpiredNudgeThrottlesLocked(maxAge time.Duration) int {
	now := time.Now()
	removed := 0
	for k, last := range sm.data.NudgeThrottles {
		if now.Sub(last) > maxAge {
			delete(sm.data.NudgeThrottles, k)
			removed++
		}
	}
	return removed
}

// --- Thread-reply DLQ (ka-0000000612) ---

// threadReplyDLQMax bounds the DLQ so a prolonged daemon outage cannot grow the
// state file without limit. When full, EnqueueThreadReplyDLQ evicts the oldest
// entry (by EnqueuedAt) to make room — the newest reply is always the most worth
// keeping, and the eviction is counted so the loss stays observable.
const threadReplyDLQMax = 500

// EnqueueThreadReplyDLQ records a failed thread reply for later replay, keyed by
// its idempotency key so a re-enqueue of the same reply coalesces (its Attempts
// counter is preserved). Returns (evicted, error): evicted is true when the queue
// was at capacity and the oldest entry was dropped to make room. Persists on every
// call so a queued reply survives a bridge crash.
func (sm *StateManager) EnqueueThreadReplyDLQ(e ThreadReplyDLQEntry) (evicted bool, err error) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if sm.data.ThreadReplyDLQ == nil {
		sm.data.ThreadReplyDLQ = make(map[string]ThreadReplyDLQEntry)
	}
	if existing, ok := sm.data.ThreadReplyDLQ[e.IdempotencyKey]; ok {
		// Preserve the original enqueue time + accumulated attempt count so a
		// re-failed replay does not reset the entry's age or retry budget.
		e.EnqueuedAt = existing.EnqueuedAt
		if e.Attempts < existing.Attempts {
			e.Attempts = existing.Attempts
		}
	} else if len(sm.data.ThreadReplyDLQ) >= threadReplyDLQMax {
		// At capacity for a genuinely new key: evict the oldest entry.
		var oldestKey string
		var oldestAt time.Time
		for k, v := range sm.data.ThreadReplyDLQ {
			if oldestKey == "" || v.EnqueuedAt.Before(oldestAt) {
				oldestKey, oldestAt = k, v.EnqueuedAt
			}
		}
		if oldestKey != "" {
			delete(sm.data.ThreadReplyDLQ, oldestKey)
			evicted = true
		}
	}
	sm.data.ThreadReplyDLQ[e.IdempotencyKey] = e
	return evicted, sm.saveLocked()
}

// AllThreadReplyDLQ returns a snapshot copy of the queued thread replies. The
// drain loop iterates this snapshot without holding the lock, dequeuing each
// entry it successfully replays.
func (sm *StateManager) AllThreadReplyDLQ() []ThreadReplyDLQEntry {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	out := make([]ThreadReplyDLQEntry, 0, len(sm.data.ThreadReplyDLQ))
	for _, e := range sm.data.ThreadReplyDLQ {
		out = append(out, e)
	}
	return out
}

// DequeueThreadReplyDLQ removes the entry with the given idempotency key (called
// after a successful replay) and persists.
func (sm *StateManager) DequeueThreadReplyDLQ(idempotencyKey string) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if _, ok := sm.data.ThreadReplyDLQ[idempotencyKey]; !ok {
		return nil
	}
	delete(sm.data.ThreadReplyDLQ, idempotencyKey)
	return sm.saveLocked()
}

// ThreadReplyDLQDepth returns the current number of queued thread replies.
func (sm *StateManager) ThreadReplyDLQDepth() int {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return len(sm.data.ThreadReplyDLQ)
}

// --- Jira inbound-comment DLQ (epic ka-0000007401) ---

// jiraInboundCommentDLQMax bounds the Jira inbound-comment DLQ so a prolonged
// daemon outage cannot grow the state file without limit. When full,
// EnqueueJiraInboundCommentDLQ evicts the oldest entry (by EnqueuedAt) to make
// room, and the eviction is counted so the loss stays observable. Mirrors
// threadReplyDLQMax.
const jiraInboundCommentDLQMax = 500

// EnqueueJiraInboundCommentDLQ records a failed inbound comment mirror for later
// replay, keyed by its (bead, comment) key so a re-enqueue of the same comment
// coalesces (its Attempts counter is preserved). Returns (evicted, error):
// evicted is true when the queue was at capacity and the oldest entry was dropped
// to make room. Persists on every call so a queued mirror survives a bridge
// crash. Mirrors EnqueueThreadReplyDLQ.
func (sm *StateManager) EnqueueJiraInboundCommentDLQ(e JiraInboundCommentDLQEntry) (evicted bool, err error) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if sm.data.JiraInboundCommentDLQ == nil {
		sm.data.JiraInboundCommentDLQ = make(map[string]JiraInboundCommentDLQEntry)
	}
	if existing, ok := sm.data.JiraInboundCommentDLQ[e.Key]; ok {
		// Preserve the original enqueue time + accumulated attempt count so a
		// re-failed replay does not reset the entry's age or retry budget.
		e.EnqueuedAt = existing.EnqueuedAt
		if e.Attempts < existing.Attempts {
			e.Attempts = existing.Attempts
		}
	} else if len(sm.data.JiraInboundCommentDLQ) >= jiraInboundCommentDLQMax {
		// At capacity for a genuinely new key: evict the oldest entry.
		var oldestKey string
		var oldestAt time.Time
		for k, v := range sm.data.JiraInboundCommentDLQ {
			if oldestKey == "" || v.EnqueuedAt.Before(oldestAt) {
				oldestKey, oldestAt = k, v.EnqueuedAt
			}
		}
		if oldestKey != "" {
			delete(sm.data.JiraInboundCommentDLQ, oldestKey)
			evicted = true
		}
	}
	sm.data.JiraInboundCommentDLQ[e.Key] = e
	return evicted, sm.saveLocked()
}

// AllJiraInboundCommentDLQ returns a snapshot copy of the queued comment mirrors.
// The drain loop iterates this snapshot without holding the lock, dequeuing each
// entry it successfully replays. Mirrors AllThreadReplyDLQ.
func (sm *StateManager) AllJiraInboundCommentDLQ() []JiraInboundCommentDLQEntry {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	out := make([]JiraInboundCommentDLQEntry, 0, len(sm.data.JiraInboundCommentDLQ))
	for _, e := range sm.data.JiraInboundCommentDLQ {
		out = append(out, e)
	}
	return out
}

// DequeueJiraInboundCommentDLQ removes the entry with the given key (called after
// a successful replay) and persists. Mirrors DequeueThreadReplyDLQ.
func (sm *StateManager) DequeueJiraInboundCommentDLQ(key string) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if _, ok := sm.data.JiraInboundCommentDLQ[key]; !ok {
		return nil
	}
	delete(sm.data.JiraInboundCommentDLQ, key)
	return sm.saveLocked()
}

// JiraInboundCommentDLQDepth returns the current number of queued comment mirrors.
func (sm *StateManager) JiraInboundCommentDLQDepth() int {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return len(sm.data.JiraInboundCommentDLQ)
}

// --- Dashboard ---

// GetDashboard returns the dashboard message ref.
func (sm *StateManager) GetDashboard() (*DashboardRef, bool) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	if sm.data.Dashboard == nil {
		return nil, false
	}
	ref := *sm.data.Dashboard
	return &ref, true
}

// SetDashboard stores the dashboard message ref and persists.
func (sm *StateManager) SetDashboard(ref DashboardRef) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.data.Dashboard = &ref
	return sm.saveLocked()
}

// --- SSE Event ID ---

// GetLastEventID returns the last processed SSE event ID.
func (sm *StateManager) GetLastEventID() string {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.data.LastEventID
}

// SetLastEventID stores the last processed SSE event ID and persists.
func (sm *StateManager) SetLastEventID(id string) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.data.LastEventID = id
	return sm.saveLocked()
}

// --- Reconcile Watermark ---

// GetLastReconcileAt returns the watermark consumed by the trigger:bead
// reconciler. Zero value indicates the reconciler has not run yet (callers
// typically bootstrap from time.Now() in that case).
func (sm *StateManager) GetLastReconcileAt() time.Time {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.data.LastReconcileAt
}

// SetLastReconcileAt advances the reconciler watermark and persists.
func (sm *StateManager) SetLastReconcileAt(t time.Time) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.data.LastReconcileAt = t
	return sm.saveLocked()
}

// --- Compaction ---

// ThreadAgentTTL is the default inactivity duration after which thread-agent
// bindings are eligible for cleanup.
const ThreadAgentTTL = 24 * time.Hour

// CompactStaleEntries removes decision/chat messages for agents that are no
// longer active, and thread-agent bindings that have exceeded the inactivity
// TTL. Agent cards are intentionally preserved so that findAgentCardByThread
// can still identify card threads after the hot cache is pruned. This prevents
// unbounded growth of the state file over weeks of operation. Returns the
// number of entries removed.
// The activeAgents set should contain short agent names of currently active agents.
func (sm *StateManager) CompactStaleEntries(activeAgents map[string]bool) (int, error) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	removed := 0

	// Compact decision messages for agents that are no longer active.
	for id, ref := range sm.data.DecisionMessages {
		if ref.Agent != "" && !activeAgents[ref.Agent] {
			delete(sm.data.DecisionMessages, id)
			removed++
		}
	}

	// Compact chat messages for agents that are no longer active.
	for id, ref := range sm.data.ChatMessages {
		if ref.Agent != "" && !activeAgents[ref.Agent] {
			delete(sm.data.ChatMessages, id)
			removed++
		}
	}

	// NOTE: Agent cards are intentionally preserved in state even after the
	// agent is no longer active. The card ref is needed so that
	// findAgentCardByThread can still identify a thread as belonging to a
	// specific agent — without it, mentions in pruned card threads would
	// spawn a new thread agent instead of respawning the original card agent.
	// Cards are only removed via explicit user action (handleClearAgent) or
	// when the Slack message no longer exists.

	// Compact thread-agent bindings that have exceeded the TTL.
	removed += sm.cleanExpiredThreadAgentsLocked(ThreadAgentTTL)

	// Clean up expired nudge throttle entries (anything older than 1h
	// is well past any throttle interval and safe to remove).
	removed += sm.cleanExpiredNudgeThrottlesLocked(time.Hour)

	if removed > 0 {
		return removed, sm.saveLocked()
	}
	return 0, nil
}

// CleanExpiredThreadAgents removes thread-agent bindings whose LastActive
// timestamp is older than the given TTL, along with their listen-thread
// flags. Returns the number of entries removed.
func (sm *StateManager) CleanExpiredThreadAgents(ttl time.Duration) (int, error) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	removed := sm.cleanExpiredThreadAgentsLocked(ttl)
	if removed > 0 {
		return removed, sm.saveLocked()
	}
	return 0, nil
}

// cleanExpiredThreadAgentsLocked removes expired thread-agent entries.
// Caller must hold sm.mu.
func (sm *StateManager) cleanExpiredThreadAgentsLocked(ttl time.Duration) int {
	now := time.Now()
	removed := 0
	for k, entry := range sm.data.ThreadAgents {
		if !entry.LastActive.IsZero() && now.Sub(entry.LastActive) > ttl {
			sm.dropBindingLocked(k)
			removed++
		}
	}
	// An alias whose head is bound to no agent any more goes too: the next
	// spawn in that thread aliases it again. The state file stays bounded.
	for tuple, head := range sm.data.ThreadHeads {
		if _, ok := sm.data.ThreadAgents[head]; !ok {
			delete(sm.data.ThreadHeads, tuple)
		}
	}
	return removed
}

// Stats returns counts of all tracked state entries (for observability).
func (sm *StateManager) Stats() (decisions, chats, cards, threads, listens int) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return len(sm.data.DecisionMessages),
		len(sm.data.ChatMessages),
		len(sm.data.AgentCards),
		len(sm.data.ThreadAgents),
		len(sm.data.ListenThreads)
}

// --- Persistence ---

func (sm *StateManager) load() error {
	data, err := os.ReadFile(sm.path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, &sm.data); err != nil {
		return fmt.Errorf("unmarshal state: %w", err)
	}
	// Ensure maps are initialized.
	if sm.data.DecisionMessages == nil {
		sm.data.DecisionMessages = make(map[string]MessageRef)
	}
	if sm.data.ChatMessages == nil {
		sm.data.ChatMessages = make(map[string]MessageRef)
	}
	if sm.data.AgentCards == nil {
		sm.data.AgentCards = make(map[string]MessageRef)
	}
	if sm.data.ThreadAgents == nil {
		sm.data.ThreadAgents = make(map[string]ThreadAgentEntry)
	}
	if sm.data.ThreadHeads == nil {
		sm.data.ThreadHeads = make(map[string]string)
	}
	if sm.data.ListenThreads == nil {
		sm.data.ListenThreads = make(map[string]bool)
	}
	if sm.data.NudgeThrottles == nil {
		sm.data.NudgeThrottles = make(map[string]time.Time)
	}
	if sm.data.ThreadReplyDLQ == nil {
		sm.data.ThreadReplyDLQ = make(map[string]ThreadReplyDLQEntry)
	}
	if sm.data.JiraInboundCommentDLQ == nil {
		sm.data.JiraInboundCommentDLQ = make(map[string]JiraInboundCommentDLQEntry)
	}
	return nil
}

// saveLocked writes state to disk atomically. Caller must hold sm.mu.
func (sm *StateManager) saveLocked() error {
	data, err := json.MarshalIndent(sm.data, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}

	dir := filepath.Dir(sm.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}

	tmp := sm.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write state tmp: %w", err)
	}
	if err := os.Rename(tmp, sm.path); err != nil {
		return fmt.Errorf("rename state: %w", err)
	}
	return nil
}
