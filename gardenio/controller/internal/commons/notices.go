package commons

import (
	"encoding/json"
	"html/template"
	"strconv"
	"strings"
	"time"
)

// Death and crash faces (Commons II step 10, spec 2.5 "death: cause, storm
// roll-up count"). The bridge writes a death report, a crash notice and, per
// land, a death-storm roll-up as kind=notice message beads with a notice field
// (internal/bridge/agent_notices.go); the face is read off the bead itself, not
// a subject, because it records what was known when the agent died.

// Notice bead fields (the bridge writes the same names).
const (
	fieldNotice         = "notice"
	fieldNoticeAgent    = "notice_agent"
	fieldNoticeCause    = "notice_cause"
	fieldNoticePod      = "notice_pod"
	fieldNoticePodPhase = "notice_pod_phase"
	fieldNoticeCount    = "notice_count"
	fieldNoticeWindow   = "notice_window"
	fieldNoticeDead     = "notice_dead"
	// The agent bead a death or crash is about. Not subject_id: the daemon
	// holds subject_id to kind=card (gd-JGmXQuSndB).
	fieldNoticeSubject = "notice_subject"
)

// noticeDeadMax caps the agents a storm roll-up names in its face; the rest
// are a count that opens on the others (the rubric summarises a list past 8,
// C11, and a cut needs a way to the rest, C1: gd-HEky8ZLUVK).
const noticeDeadMax = 8

// noticeRestMax bounds the names behind the count, as the bridge bounds the
// deaths it remembers by name (deathStormHeldMax).
const noticeRestMax = 200

// Notice is a death report's, crash notice's or storm roll-up's face.
type Notice struct {
	Kind    string // death | crash | death_storm
	Agent   string // the agent's name (death, crash)
	Subject string // the agent bead (death, crash)
	Cause   string // death
	Pod     string // crash
	Phase   string // crash: the pod's phase
	// The storm roll-up: how many deaths the land's reports held back, in what
	// window, and the first noticeDeadMax of them by name.
	Count  int
	Window string
	Dead   []NoticeAgent
	More   int // held-back deaths not named in Dead
	// Rest are the named ones among More, behind the count's disclosure, and
	// Unnamed the others (the report named only so many).
	Rest    []NoticeAgent
	Unnamed int
}

// NoticeAgent is a dead agent a storm roll-up names.
type NoticeAgent struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// noticeMarks are the notice faces' marks, as inline SVG for the same reason
// as stateMarks: a symbol glyph is not in the pinned Noto subsets.
var noticeMarks = map[string]template.HTML{
	"death": stateMarks["failed"],
	// a warning triangle with a bar and a dot
	"warn": markSVG(`<path d="M8 2.2L14.2 13H1.8z" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linejoin="round"/><path d="M8 6.2v3.3" stroke="currentColor" stroke-width="1.5" stroke-linecap="round"/><circle cx="8" cy="11.4" r=".9" fill="currentColor"/>`),
}

// Mark is the face's mark: a cross for a death, a warning for a crash or a
// storm.
func (n Notice) Mark() template.HTML {
	if n.Kind == "death" {
		return noticeMarks["death"]
	}
	return noticeMarks["warn"]
}

// noticeFrom reads a notice face off a notice bead's fields; nil for a bead
// that is not a death, crash or storm notice, which renders as plain text.
func noticeFrom(f map[string]string) *Notice {
	subject := strings.TrimSpace(f[fieldNoticeSubject])
	n := &Notice{Kind: strings.TrimSpace(f[fieldNotice]), Subject: subject}
	switch n.Kind {
	case "death", "crash":
		n.Agent = clipRunes(strings.TrimSpace(f[fieldNoticeAgent]), 200)
		if n.Agent == "" {
			n.Agent = subject
		}
		if n.Agent == "" {
			return nil
		}
		n.Cause = clipRunes(strings.TrimSpace(f[fieldNoticeCause]), 2000)
		n.Pod = clipRunes(strings.TrimSpace(f[fieldNoticePod]), 200)
		n.Phase = clipRunes(strings.TrimSpace(f[fieldNoticePodPhase]), 50)
	case "death_storm":
		n.Count, _ = strconv.Atoi(strings.TrimSpace(f[fieldNoticeCount]))
		if d, err := time.ParseDuration(strings.TrimSpace(f[fieldNoticeWindow])); err == nil && d >= time.Minute {
			n.Window = count(strconv.Itoa(int(d/time.Minute)), "minute", "minutes")
		}
		var dead []NoticeAgent
		if raw := f[fieldNoticeDead]; raw != "" {
			_ = json.Unmarshal([]byte(raw), &dead)
		}
		for _, a := range dead {
			a.ID, a.Name = strings.TrimSpace(a.ID), clipRunes(strings.TrimSpace(a.Name), 200)
			if a.Name == "" {
				a.Name = a.ID
			}
			if a.Name != "" {
				n.Dead = append(n.Dead, a)
			}
		}
		if n.Count < len(n.Dead) {
			n.Count = len(n.Dead)
		}
		if n.Count <= 0 {
			return nil
		}
		if len(n.Dead) > noticeDeadMax {
			n.Dead, n.Rest = n.Dead[:noticeDeadMax], n.Dead[noticeDeadMax:min(len(n.Dead), noticeDeadMax+noticeRestMax)]
		}
		n.More = n.Count - len(n.Dead)
		n.Unnamed = n.More - len(n.Rest)
	default:
		return nil
	}
	return n
}
