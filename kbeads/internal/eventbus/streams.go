package eventbus

import (
	"fmt"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
)

const (
	StreamHookEvents     = "HOOK_EVENTS"
	StreamDecisionEvents = "DECISION_EVENTS"
	StreamAgentEvents    = "AGENT_EVENTS"
	StreamMailEvents     = "MAIL_EVENTS"
	StreamMutationEvents = "MUTATION_EVENTS"
	StreamConfigEvents   = "CONFIG_EVENTS"
	StreamGateEvents     = "GATE_EVENTS"
	StreamInboxEvents    = "INBOX_EVENTS"
	StreamJackEvents     = "JACK_EVENTS"
	StreamMessageEvents  = "MESSAGE_EVENTS"
	StreamPlanEvents     = "PLAN_EVENTS"

	SubjectHookPrefix     = "hooks."
	SubjectDecisionPrefix = "decisions."
	SubjectAgentPrefix    = "agents."
	SubjectMailPrefix     = "mail."
	SubjectMutationPrefix = "mutations."
	SubjectConfigPrefix   = "config."
	SubjectGatePrefix     = "gate."
	SubjectInboxPrefix    = "inbox."
	SubjectJackPrefix     = "jack."
	SubjectMessagePrefix  = "coop.events.messages."
	SubjectPlanPrefix     = "plan."
)

// StreamNames lists all known stream short names.
var StreamNames = []string{
	"hooks", "decisions", "agents", "mail",
	"mutations", "config", "gate", "inbox", "jack", "messages", "plan",
}

var streamToPrefix = map[string]string{
	"hooks":     SubjectHookPrefix,
	"decisions": SubjectDecisionPrefix,
	"agents":    SubjectAgentPrefix,
	"mail":      SubjectMailPrefix,
	"mutations": SubjectMutationPrefix,
	"config":    SubjectConfigPrefix,
	"gate":      SubjectGatePrefix,
	"inbox":     SubjectInboxPrefix,
	"jack":      SubjectJackPrefix,
	"messages":  SubjectMessagePrefix,
	"plan":      SubjectPlanPrefix,
}

var prefixToStream = map[string]string{
	"hooks":                "hooks",
	"decisions":            "decisions",
	"agents":               "agents",
	"mail":                 "mail",
	"mutations":            "mutations",
	"config":               "config",
	"gate":                 "gate",
	"inbox":                "inbox",
	"jack":                 "jack",
	"coop.events.messages": "messages",
	"plan":                 "plan",
}

// StreamForSubject returns the short stream name for a NATS subject.
func StreamForSubject(subject string) string {
	// Check multi-segment prefixes first (e.g., "coop.events.messages").
	for prefix, name := range prefixToStream {
		if strings.HasPrefix(subject, prefix+".") || strings.HasPrefix(subject, prefix) {
			return name
		}
	}
	return ""
}

// EventTypeFromSubject extracts the event type from a NATS subject (last segment).
func EventTypeFromSubject(subject string) string {
	for i := len(subject) - 1; i >= 0; i-- {
		if subject[i] == '.' {
			return subject[i+1:]
		}
	}
	return subject
}

// SubjectPrefixForStream returns the NATS subject prefix for a short stream name.
func SubjectPrefixForStream(name string) string {
	return streamToPrefix[name]
}

// SubjectForEvent returns the NATS subject for a given event type.
func SubjectForEvent(eventType EventType) string {
	if eventType.IsDecisionEvent() {
		return SubjectDecisionPrefix + string(eventType)
	}
	if eventType.IsAgentEvent() {
		return SubjectAgentPrefix + string(eventType)
	}
	if eventType.IsMailEvent() {
		return SubjectMailPrefix + string(eventType)
	}
	if eventType.IsMutationEvent() {
		return SubjectMutationPrefix + string(eventType)
	}
	if eventType.IsConfigEvent() {
		return SubjectConfigPrefix + string(eventType)
	}
	if eventType.IsGateEvent() {
		return SubjectGatePrefix + string(eventType)
	}
	if eventType.IsJackEvent() {
		return SubjectJackPrefix + string(eventType)
	}
	if eventType.IsMessageEvent() {
		return SubjectMessagePrefix + string(eventType)
	}
	return SubjectHookPrefix + string(eventType)
}

// SubjectForDecisionEvent returns a scoped NATS subject for a decision event.
func SubjectForDecisionEvent(eventType EventType, requestedBy string) string {
	scope := "_global"
	if requestedBy != "" {
		scope = requestedBy
	}
	return SubjectDecisionPrefix + scope + "." + string(eventType)
}

// SubjectForHookEvent returns an agent-scoped NATS subject for a hook event.
func SubjectForHookEvent(eventType EventType, actor string) string {
	scope := "_global"
	if actor != "" {
		scope = actor
	}
	return SubjectHookPrefix + scope + "." + string(eventType)
}

// SubjectForMessageEvent returns an agent-scoped NATS subject for a message event.
func SubjectForMessageEvent(eventType EventType, actor string) string {
	scope := "_global"
	if actor != "" {
		scope = actor
	}
	return SubjectMessagePrefix + scope + "." + string(eventType)
}

// Default retention applied to every stream that does not override it below.
// A count + byte cap with DiscardOld eviction; no per-subject floor and no age
// ceiling. This is the historical config all ten streams shared.
const (
	defaultStreamMaxMsgs  int64 = 10000
	defaultStreamMaxBytes int64 = 100 << 20 // 100MB
)

// MESSAGE_EVENTS retention (per-subject floor). Unlike the other nine streams,
// MESSAGE_EVENTS is a SHARED fleet-wide stream carrying assistant_text +
// thinking + usage for EVERY agent (subject coop.events.messages.<actor>.<type>,
// so each of the three event types is already a distinct subject per agent). Its
// durable purpose is the full-tail assistant_text replay a viewer runs after a
// multi-turn outage (assistant_turn_minter.go). Under the old shared count/byte
// caps with DiscardOld, the whole fleet's thinking traces (≤256KB each, the
// volume driver) share ONE oldest-first eviction budget with every plan's
// assistant_text, so a busy fleet can evict the intermediate turns of an outage
// before any replay runs — the "honest completeness floor" the replay design
// (ka-0000007237) called a hard prerequisite.
//
// FIX (Option 1, per-subject retention — chosen over a dedicated stream or
// MaxAge-only after a judge panel + empirical JetStream probing, see
// ka-0000003251): the binding constraint is the GLOBAL byte/count cap with
// DiscardOld, which evicts the globally-oldest message across ALL subjects and
// PREEMPTS MaxMsgsPerSubject (a tight global cap evicts a quiet subject below its
// own per-subject floor). So the durable floor requires ALL of:
//   - MaxMsgsPerSubject: a real per-subject floor — each agent-subject retains N
//     recent messages independent of fleet volume. assistant_text / thinking /
//     usage are distinct subjects, so each keeps its own N.
//   - MaxMsgs = -1 (unlimited): drop the global COUNT cap, which has the same
//     oldest-across-all-subjects preemption problem as the byte cap.
//   - MaxBytes raised to a PVC-safe backstop: sized so the realistic sum of
//     per-subject working sets stays under it (per-subject retention actually
//     governs), while still fitting the shared NATS JetStream PVC alongside the
//     other nine streams. An unlimited byte cap would be disk-unsafe here.
//   - MaxAge: a dead-subject reaper and outage-survival window — longer than any
//     realistic viewer outage, so a quiet/dead agent's subjects are eventually
//     reclaimed rather than pinned forever by the per-subject floor.
const (
	messageStreamMaxMsgsPerSubject int64         = 500
	messageStreamMaxBytes          int64         = 512 << 20 // 512MB PVC-safe backstop
	messageStreamMaxAge            time.Duration = 72 * time.Hour
)

// PLAN_EVENTS retention + dedupe (producer half of P3, ka-0000004264). The plan
// step-added signal is the reactive-collaboration trigger — a consumer restart
// must not lose a step edge (at-least-once), and a duplicate re-emit (the daemon
// double-writing core-NATS + JetStream, a reconnect replay, or the same edge
// landing twice on an ON CONFLICT DO NOTHING insert) must NOT fire the nudge
// twice. JetStream gives us both:
//
//   - Persistence (FileStorage) + a generous count/age budget so a parked or
//     briefly-down controller reactor replays every missed step on reconnect.
//   - A Duplicates dedupe window: the daemon stamps a deterministic Nats-Msg-Id
//     per (plan, step) edge (see server emit site), and JetStream drops a second
//     publish of the same id landing inside the window. The window must comfortably
//     exceed the daemon's own double-write gap AND a realistic consumer-reconnect
//     replay, so a redelivery of the identical message id is still recognized as a
//     duplicate rather than re-appended.
//
// Volume is low (one message per plan step add — human/agent-paced, not
// machine-firehose), so the historical count/byte caps are ample; the age cap is
// the dead-plan reaper.
const (
	planStreamMaxMsgs    int64         = defaultStreamMaxMsgs
	planStreamMaxBytes   int64         = defaultStreamMaxBytes
	planStreamMaxAge     time.Duration = 72 * time.Hour
	planStreamDuplicates time.Duration = 10 * time.Minute
)

// streamSpec is the desired config for one JetStream stream. A zero
// maxMsgsPerSubject means "no per-subject limit"; config() emits it as the
// canonical -1 (unlimited) because the JetStream server itself stores an unset
// per-subject cap as -1 and reports it back that way. Emitting -1 up front keeps
// a freshly-created stream from reading back as DRIFTED on the next boot (a 0-vs-1
// mismatch would trigger a spurious UpdateStream on all nine unchanged streams
// every boot). maxAge==0 means no age ceiling and is NOT normalized (the server
// reports 0 back as 0). A zero duplicates means "no dedupe window" — config()
// leaves Duplicates unset, matching every stream that predates the dedupe
// feature; a non-zero value opts a stream into JetStream message-id dedupe.
type streamSpec struct {
	name              string
	subject           string
	maxMsgs           int64
	maxBytes          int64
	maxMsgsPerSubject int64
	maxAge            time.Duration
	duplicates        time.Duration
}

func (s streamSpec) config() *nats.StreamConfig {
	maxMsgsPerSubject := s.maxMsgsPerSubject
	if maxMsgsPerSubject == 0 {
		maxMsgsPerSubject = -1 // canonical "unlimited" the server stores + reports
	}
	return &nats.StreamConfig{
		Name:              s.name,
		Subjects:          []string{s.subject},
		Storage:           nats.FileStorage,
		MaxMsgs:           s.maxMsgs,
		MaxBytes:          s.maxBytes,
		MaxMsgsPerSubject: maxMsgsPerSubject,
		MaxAge:            s.maxAge,
		Discard:           nats.DiscardOld,
		Duplicates:        s.duplicates,
	}
}

// streamSpecs is the desired state for every stream. The base streams keep the
// historical count/byte caps; MESSAGE_EVENTS gets the per-subject retention
// floor; PLAN_EVENTS adds a Duplicates dedupe window for at-least-once + dedupe.
var streamSpecs = []streamSpec{
	{name: StreamHookEvents, subject: SubjectHookPrefix + ">", maxMsgs: defaultStreamMaxMsgs, maxBytes: defaultStreamMaxBytes},
	{name: StreamDecisionEvents, subject: SubjectDecisionPrefix + ">", maxMsgs: defaultStreamMaxMsgs, maxBytes: defaultStreamMaxBytes},
	{name: StreamAgentEvents, subject: SubjectAgentPrefix + ">", maxMsgs: defaultStreamMaxMsgs, maxBytes: defaultStreamMaxBytes},
	{name: StreamMailEvents, subject: SubjectMailPrefix + ">", maxMsgs: defaultStreamMaxMsgs, maxBytes: defaultStreamMaxBytes},
	{name: StreamMutationEvents, subject: SubjectMutationPrefix + ">", maxMsgs: defaultStreamMaxMsgs, maxBytes: defaultStreamMaxBytes},
	{name: StreamConfigEvents, subject: SubjectConfigPrefix + ">", maxMsgs: defaultStreamMaxMsgs, maxBytes: defaultStreamMaxBytes},
	{name: StreamGateEvents, subject: SubjectGatePrefix + ">", maxMsgs: defaultStreamMaxMsgs, maxBytes: defaultStreamMaxBytes},
	{name: StreamInboxEvents, subject: SubjectInboxPrefix + ">", maxMsgs: defaultStreamMaxMsgs, maxBytes: defaultStreamMaxBytes},
	{name: StreamJackEvents, subject: SubjectJackPrefix + ">", maxMsgs: defaultStreamMaxMsgs, maxBytes: defaultStreamMaxBytes},
	{
		name:              StreamMessageEvents,
		subject:           SubjectMessagePrefix + ">",
		maxMsgs:           -1, // unlimited: the count cap preempts the per-subject floor
		maxBytes:          messageStreamMaxBytes,
		maxMsgsPerSubject: messageStreamMaxMsgsPerSubject,
		maxAge:            messageStreamMaxAge,
	},
	{
		name:       StreamPlanEvents,
		subject:    SubjectPlanPrefix + ">",
		maxMsgs:    planStreamMaxMsgs,
		maxBytes:   planStreamMaxBytes,
		maxAge:     planStreamMaxAge,
		duplicates: planStreamDuplicates,
	},
}

// retentionDrifted reports whether a live stream's retention config differs from
// the desired spec on any field EnsureStreams manages. It is intentionally
// narrow — only the retention knobs, not every StreamConfig field — so
// server-defaulted or operator-tuned fields we do not manage never trigger a
// spurious UpdateStream on every boot. Subjects are compared too so a narrowing
// (needed only if a future dedicated-stream migration lands) would reconcile.
func retentionDrifted(live nats.StreamConfig, want *nats.StreamConfig) bool {
	if live.MaxMsgs != want.MaxMsgs ||
		live.MaxBytes != want.MaxBytes ||
		live.MaxMsgsPerSubject != want.MaxMsgsPerSubject ||
		live.MaxAge != want.MaxAge ||
		live.Discard != want.Discard {
		return true
	}
	// Duplicates is only compared for streams that MANAGE it (want.Duplicates
	// non-zero, e.g. PLAN_EVENTS). The JetStream server defaults an unset
	// Duplicates to a non-zero value (2m) and reports that back, so an
	// unconditional compare would report false drift on every stream that does
	// NOT set it — the same server-default trap the MaxMsgsPerSubject 0-vs-1
	// normalization avoids. A stream we manage keeps its window reconciled; the
	// rest are left to the server default untouched.
	if want.Duplicates != 0 && live.Duplicates != want.Duplicates {
		return true
	}
	if len(live.Subjects) != len(want.Subjects) {
		return true
	}
	for i := range want.Subjects {
		if live.Subjects[i] != want.Subjects[i] {
			return true
		}
	}
	return false
}

// EnsureStreams reconciles the required JetStream streams to their desired
// retention config: it CREATES any stream that does not yet exist and UPDATES an
// existing stream only when its retention has DRIFTED from the spec.
//
// This replaces the historical create-only behavior. Create-only meant a
// retention change (e.g. the MESSAGE_EVENTS per-subject floor) landed only on a
// FRESH NATS store — an existing prod stream kept its old config forever. The
// drift-gated UpdateStream lands the change on existing streams too, while
// leaving the nine unchanged streams untouched on every boot (no spurious
// updates). UpdateStream here is purely relaxing for MESSAGE_EVENTS (retains
// strictly more), so it is non-destructive: messages already stored survive.
func EnsureStreams(js nats.JetStreamContext) error {
	for _, s := range streamSpecs {
		want := s.config()
		info, err := js.StreamInfo(s.name)
		if err != nil {
			// Stream absent (or unreachable) — create it. AddStream surfaces a
			// real connection error if the server is genuinely unreachable.
			if _, aerr := js.AddStream(want); aerr != nil {
				return fmt.Errorf("create %s stream: %w", s.name, aerr)
			}
			continue
		}
		if retentionDrifted(info.Config, want) {
			if _, uerr := js.UpdateStream(want); uerr != nil {
				return fmt.Errorf("update %s stream retention: %w", s.name, uerr)
			}
		}
	}
	return nil
}
