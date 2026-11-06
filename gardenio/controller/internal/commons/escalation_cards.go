package commons

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"gardenio/controller/internal/beadsapi"
)

// Escalation card face (spec 2.5, Commons II step 4). While the escalation
// waits it shows what is asked (the operation, where it runs, its
// sensitivity, the credentials it needs, the context and the work it blocks)
// with Approve and Reject when the daemon says the viewer may approve it
// (EscalationActionsFor), and a Review link to the escalation's page, where
// it can be approved today. Once approved it shows who approved it while it
// runs; once closed, the result or the rejection.
//
// Approve and Reject post to the card action route, which Commons II step 5
// (slice 4a) carries to the bridge's inbound endpoint, like a decision card's
// buttons (decision_cards.go).

// Bounds on what an escalation card shows of its subject.
const (
	cardPingReasonMax = 300 // the last pingback's reason
	cardCredsShown    = 4   // credentials named; the rest are counted
)

// EscalationCard is an escalation card's face.
type EscalationCard struct {
	ID          string // the escalation bead
	Land        string
	Priority    int
	Title       string
	Operation   string // operation_type
	Target      string // execution_target
	Sensitivity string
	Creds       []string // the credential types it needs, at most cardCredsShown
	MoreCreds   int      // how many more it needs
	Context     string
	Script      bool   // an execution script is attached
	Asker       string // the name of the agent that filed it, never a bare bead id (withCards resolves it)
	// Audience is who the escalation is for: owner, operator or desk
	// (beadsapi.EscalationAudience). An owner one is the owner's to act on;
	// the others are marked in hand.
	Audience string
	// HeldBy is the lane that holds it (escalation-routing D1), WaitingOn its
	// plain-words label (beadsapi.LaneLabel). Both empty for an unstamped
	// escalation, which then shows only Audience (Global Constraint 7).
	HeldBy    string
	WaitingOn string
	// CanApprove is the daemon's can-approve for this viewer (D9); only then
	// do Approve and Reject render. Set by hydration, never by escalationCardFrom.
	CanApprove bool
	// Waiting is who the open escalation waits on, in words for this viewer:
	// "you" when they may approve it, else its lane or audience (waiting).
	// Set by hydration, after CanApprove.
	Waiting    string
	Task       string // the task it blocks
	MR         string // the merge request it asks to merge, when it names one
	Pingbacks  int
	PingReason string
	// State is waiting, approved (running), done or rejected.
	State string
	// The approved and closed faces.
	By       string // who approved, or who rejected
	Executor string
	Result   string // success | failure | partial
	Summary  string // the redacted summary of a sensitive result
	Reason   string // why it was rejected
}

// EscalationActionsFor reports whether the viewer on ctx may approve (and so
// reject) card's escalation: the daemon's can-approve on the viewer's own
// bearer (D9). False for a closed card, a nil Approvals or any error (fail
// closed; the error is logged). Whoever may approve may reject.
func EscalationActionsFor(ctx context.Context, a Approvals, card *EscalationCard) bool {
	if a == nil || card == nil || !card.Open() {
		return false
	}
	res, err := a.CanApproveEscalation(ctx, card.ID)
	if err != nil {
		slog.Warn("commons: can-approve failed; hiding escalation buttons", "escalation", card.ID, "error", err)
		return false
	}
	return res.Allowed
}

// Urgency is the priority as the card colours it.
func (c EscalationCard) Urgency() string {
	return DecisionCard{Priority: c.Priority}.Urgency()
}

// waiting is Waiting: "" once the escalation no longer waits.
func (c EscalationCard) waiting() string {
	switch {
	case !c.Open():
		return ""
	case c.CanApprove:
		return "you"
	case c.WaitingOn != "":
		return c.WaitingOn
	case c.Audience == beadsapi.AudienceOperator:
		return "an operator"
	case c.Audience == beadsapi.AudienceDesk:
		return "the merge desk"
	}
	return "the owner"
}

// Open reports whether the escalation still waits for an operator.
func (c EscalationCard) Open() bool { return c.State == "waiting" }

// Sensitive reports whether the escalation's result is withheld from cards.
func (c EscalationCard) Sensitive() bool {
	return c.Sensitivity == "sensitive" || c.Sensitivity == "secret"
}

// escalationCardFrom reads an escalation card's face off its subject bead. ok
// is false when b is not an escalation of land.
func escalationCardFrom(b *beadsapi.BeadDetail, land string) (EscalationCard, bool) {
	if b == nil || b.Type != "escalation" || landOf(b.Labels) != land {
		return EscalationCard{}, false
	}
	f := b.Fields
	trim := func(k string) string { return strings.TrimSpace(f[k]) }
	c := EscalationCard{
		ID: b.ID, Land: land, Priority: b.Priority,
		Title:       clipRunes(strings.TrimSpace(b.Title), cardPromptMax),
		Operation:   trim("operation_type"),
		Target:      trim("execution_target"),
		Sensitivity: strings.ToLower(trim("sensitivity")),
		Creds:       escalationCreds(f["credential_requirements"]),
		Context:     clipRunes(trim("context"), cardContextMax),
		Script:      trim("execution_script") != "",
		Asker:       trim("requesting_agent"),
		Audience:    beadsapi.EscalationAudience(f),
		HeldBy:      beadsapi.EscalationHeldBy(b),
		WaitingOn:   beadsapi.LaneLabel(beadsapi.EscalationHeldBy(b)),
		Task:        trim("requesting_task"),
		MR:          escalationMR(f["operation_spec"]),
		PingReason:  clipRunes(trim("pingback_reason"), cardPingReasonMax),
		Executor:    trim("executor"),
		Result:      trim("result_status"),
	}
	if c.Asker != "" {
		c.Asker = authorName(c.Asker)
		if beadIDShape.MatchString(c.Asker) {
			c.Asker = "" // an agent bead's id reads as the escalation's own; withCards names it
		}
	}
	if n := len(c.Creds); n > cardCredsShown {
		c.Creds, c.MoreCreds = c.Creds[:cardCredsShown], n-cardCredsShown
	}
	c.Pingbacks, _ = strconv.Atoi(trim("pingback_count"))
	approvedBy := firstNonEmpty(f["approved_by_email"], f["approved_by"])
	rejectedBy := trim("rejected_by")
	c.Reason = clipRunes(firstNonEmpty(f["rejection_reason"], f["reject_reason"]), cardContextMax)
	closed := b.Status == "closed"
	// The bridge's rule (escalations.go handleClosed): a closed escalation
	// that was never approved and has no completed result was rejected.
	completed := approvedBy != "" || c.Result == "success" || c.Result == "partial"
	switch {
	case rejectedBy != "" || (closed && !completed):
		c.State, c.By = "rejected", rejectedBy
	case closed:
		c.State, c.By = "done", approvedBy
		if c.Sensitive() {
			c.Summary = clipRunes(trim("redacted_summary"), cardContextMax)
		}
	case approvedBy != "":
		c.State, c.By = "approved", approvedBy
	default:
		c.State = "waiting"
	}
	if c.By != "" {
		if !strings.Contains(c.By, ":") && strings.Contains(c.By, "@") {
			c.By = "oidc:" + c.By // approved_by_email is a bare email
		}
		c.By = authorName(c.By)
	}
	return c, true
}

// InHand reports whether the escalation is not the owner's: the desk or the
// operators have it, so its row is marked in hand.
func (c EscalationCard) InHand() bool { return c.Audience != beadsapi.AudienceOwner }

// beadIDShape is a whole bead id: two letters, a dash, ten alphanumerics.
var beadIDShape = regexp.MustCompile(`^[a-z]{2}-[A-Za-z0-9]{10}$`)

// askerName is the name of the agent that filed an escalation, from its
// requesting_agent: the agent bead's name when that is an agent of land the
// viewer may read, the value itself when it is already a name, and "" when it
// is a bead id that names no agent here (gd-uHO0OUzcl5: the header showed the
// asker's agent-bead id, which read as the escalation's own).
func askerName(ctx context.Context, rd BeadGetter, requestingAgent, land string) string {
	raw := strings.TrimSpace(requestingAgent)
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, ":") {
		if a, err := rd.GetBead(ctx, raw); err == nil && a != nil && a.ID == raw && a.Type == "agent" && landOf(a.Labels) == land {
			return firstNonEmpty(strings.TrimSpace(a.Fields["agent"]), strings.TrimSpace(a.Title))
		}
	}
	if name := authorName(raw); !beadIDShape.MatchString(name) {
		return name
	}
	return ""
}

// Where is where the escalation runs, in words.
func (c EscalationCard) Where() string {
	switch c.Target {
	case "":
		return ""
	case "local":
		return "runs locally"
	case "server":
		return "runs on the server"
	case "service_account":
		return "runs as a service account"
	}
	return "runs on " + c.Target
}

// escalationCreds is the credential types an escalation needs, as the Slack
// card summarises credential_requirements.
func escalationCreds(raw string) []string {
	var creds []struct {
		Type       string `json:"type"`
		Identifier string `json:"identifier"`
	}
	if json.Unmarshal([]byte(strings.TrimSpace(raw)), &creds) != nil {
		return nil
	}
	var out []string
	for _, c := range creds {
		if label := firstNonEmpty(c.Type, c.Identifier); label != "" {
			out = append(out, label)
		}
	}
	return out
}

// escalationMR is the merge request an escalation's operation_spec names
// (mr_url, as gb escalation create --operation-type gitlab_mr_merge writes
// it), or "" when it names none or the URL is not an absolute http(s) URL.
func escalationMR(spec string) string {
	var m struct {
		MRURL string `json:"mr_url"`
	}
	if json.Unmarshal([]byte(strings.TrimSpace(spec)), &m) != nil {
		return ""
	}
	u, err := url.Parse(strings.TrimSpace(m.MRURL))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return ""
	}
	return u.String()
}
