package landmap

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"gardenio/kbeads/internal/model"
	"gardenio/lands/contract/landsv1"
)

// beadRecord is the part of a daemon bead the map decodes. Its JSON keys are
// TypeRows keys (TestRecords_KeysAreInTables); fields stays raw and is read
// one allowlisted key at a time through field.
type beadRecord struct {
	ID          string          `json:"id"`
	Type        model.BeadType  `json:"type"`
	Title       string          `json:"title"`
	Description string          `json:"description"`
	Status      model.Status    `json:"status"`
	Priority    int             `json:"priority"`
	Assignee    string          `json:"assignee"`
	Labels      []string        `json:"labels"`
	CreatedBy   string          `json:"created_by"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
	Fields      json.RawMessage `json:"fields"`
	// Heraldry is the bead's arms in the canonical form, passed through as
	// the contract's opaque heraldry; "" is none.
	Heraldry string `json:"heraldry"`
	// Dependencies are a bead's edges as GET /v1/beads/{id} answers them;
	// OpenBlockers its open blockers as GET /v1/beads answers them with
	// with_open_blockers=true (nil when not asked). Read only to tell whether
	// a ticket is waiting (Map.waiting); never projected.
	Dependencies []depRecord      `json:"dependencies"`
	OpenBlockers *[]blockerRecord `json:"open_blockers"`
}

// depRecord is one dependency edge of a bead: it depends on DependsOnID.
type depRecord struct {
	DependsOnID string               `json:"depends_on_id"`
	Type        model.DependencyType `json:"type"`
}

// blockerRecord is one open blocker of a listed bead. OutOfScope is set for
// a blocker outside the caller's project scope.
type blockerRecord struct {
	ID         string         `json:"id"`
	Type       model.BeadType `json:"type"`
	OutOfScope bool           `json:"out_of_scope"`
}

// beadPage is GET /v1/beads.
type beadPage struct {
	Beads []beadRecord `json:"beads"`
	Total int          `json:"total"`
}

// commentRecord is a daemon comment; its keys are commentKeys.
type commentRecord struct {
	ID                int64     `json:"id"`
	Author            string    `json:"author"`
	Text              string    `json:"text"`
	CreatedAt         time.Time `json:"created_at"`
	OperatedByDisplay string    `json:"operated_by_display"`
}

// meRecord is GET /v1/me; its keys are meKeys.
type meRecord struct {
	OperatorID string `json:"operator_id"`
	Lands      []struct {
		Name string `json:"name"`
		Tier string `json:"tier"`
	} `json:"lands"`
}

// membersRecord is GET /v1/lands/{land}/members; its keys are memberKeys.
type membersRecord struct {
	Members []struct {
		Principal string `json:"principal"`
		Name      string `json:"name"`
		Role      string `json:"role"`
	} `json:"members"`
}

// inLand reports whether the bead carries the land's project label.
func (b *beadRecord) inLand(land string) bool {
	return slices.Contains(b.Labels, "project:"+land)
}

// field returns fields.<key>, or nil when it is absent. A key outside the
// bead type's TypeRow is never read.
func (b *beadRecord) field(key string) json.RawMessage {
	row, ok := RowFor(b.Type)
	if !ok || !slices.Contains(row.Fields, "fields."+key) {
		slog.Error("landmap: read of a field outside TypeRows", "type", string(b.Type), "key", key)
		return nil
	}
	if len(b.Fields) == 0 {
		return nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b.Fields, &m); err != nil {
		return nil
	}
	return m[key]
}

// fieldString reads a string field. A field stored as another JSON scalar
// reads as its literal text.
func (b *beadRecord) fieldString(key string) string {
	raw := b.field(key)
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return strings.TrimSpace(string(raw))
}

// fieldBool reads a boolean field, honouring "true" stored as a string.
func (b *beadRecord) fieldBool(key string) bool {
	switch strings.Trim(strings.TrimSpace(string(b.field(key))), `"`) {
	case "true":
		return true
	}
	return false
}

// Projections: bead record to contract DTO. Status enums are mapped, never
// copied; free text goes through the prose scrub (scrub.go).

func isTicketType(t model.BeadType) bool { return slices.Contains(ticketTypes, t) }

// ticketStatus maps a bead status. A ticket waiting on something the land
// can see (Map.waiting) reads waiting whatever its own status, short of
// closed; one blocked on anything else reads open, as blocked and deferred
// do.
func ticketStatus(s model.Status, waiting bool) landsv1.TicketStatus {
	switch {
	case s == model.StatusClosed:
		return landsv1.TicketStatusDone
	case waiting:
		return landsv1.TicketStatusWaiting
	case s == model.StatusInProgress:
		return landsv1.TicketStatusInProgress
	}
	return landsv1.TicketStatusOpen
}

// ticketStatusFilter is the bead statuses a ticket status lists from. Open,
// in progress and waiting depend on blockers too, so the page keeps only
// the tickets that read as the status asked for (ticketPage).
func ticketStatusFilter(s landsv1.TicketStatus) []model.Status {
	switch s {
	case landsv1.TicketStatusOpen:
		return []model.Status{model.StatusOpen, model.StatusBlocked, model.StatusDeferred}
	case landsv1.TicketStatusInProgress:
		return []model.Status{model.StatusInProgress}
	case landsv1.TicketStatusWaiting:
		return []model.Status{model.StatusOpen, model.StatusInProgress, model.StatusBlocked, model.StatusDeferred}
	case landsv1.TicketStatusDone:
		return []model.Status{model.StatusClosed}
	}
	return nil
}

// ticketPriority maps a bead priority (0 critical .. 4) onto 1..4.
func ticketPriority(p int) landsv1.Priority {
	switch {
	case p <= 1:
		return 1
	case p >= 4:
		return 4
	}
	return p
}

func toTicket(b *beadRecord, waiting bool) landsv1.Ticket {
	t := landsv1.Ticket{
		Id:        b.ID,
		Kind:      landsv1.TicketKind(b.Type),
		Title:     scrubProse(b.ID, b.Title),
		Status:    ticketStatus(b.Status, waiting),
		Priority:  ticketPriority(b.Priority),
		UpdatedAt: stamp(b.UpdatedAt),
	}
	if b.Description != "" {
		t.Body = scrubOpt(b.ID, &b.Description)
	}
	if a := displayName(b.Assignee); a != "" {
		t.Assignee = &a
	}
	t.Heraldry = arms(b)
	return t
}

// arms is a bead's heraldry as the contract carries it: absent for none.
func arms(b *beadRecord) *landsv1.Heraldry {
	if b.Heraldry == "" {
		return nil
	}
	h := b.Heraldry
	return &h
}

type decisionOption struct {
	ID           string `json:"id"`
	Label        string `json:"label"`
	Short        string `json:"short"`
	Description  string `json:"description"`
	ArtifactType string `json:"artifact_type"`
}

// decisionOptions reads fields.options, stored either as a JSON array or as
// a string holding one.
func decisionOptions(b *beadRecord) []decisionOption {
	raw := b.field("options")
	var s string
	if json.Unmarshal(raw, &s) == nil {
		raw = json.RawMessage(s)
	}
	var opts []decisionOption
	if json.Unmarshal(raw, &opts) != nil {
		return nil
	}
	return opts
}

// decisionStatus: closed with chosen "dismissed" (or canceled) is cancelled,
// closed otherwise is resolved.
func decisionStatus(b *beadRecord) landsv1.DecisionStatus {
	if b.Status != model.StatusClosed {
		return landsv1.DecisionStatusOpen
	}
	if b.fieldBool("canceled") || b.fieldString("chosen") == "dismissed" {
		return landsv1.DecisionStatusCancelled
	}
	return landsv1.DecisionStatusResolved
}

func toDecision(b *beadRecord) landsv1.Decision {
	d := landsv1.Decision{
		Id:       b.ID,
		Question: b.fieldString("prompt"),
		Status:   decisionStatus(b),
		OpenedAt: stamp(b.CreatedAt),
		Options:  []landsv1.DecisionOption{},
	}
	if d.Question == "" {
		d.Question = b.Title
	}
	d.Question = scrubProse(b.ID, d.Question)
	if c := b.fieldString("context"); c != "" {
		d.Context = scrubOpt(b.ID, &c)
	}
	for _, o := range decisionOptions(b) {
		if o.ID == "" {
			continue
		}
		label := o.Label
		if label == "" {
			label = o.Short
		}
		if label == "" {
			label = o.ID
		}
		opt := landsv1.DecisionOption{Id: o.ID, Label: scrubProse(b.ID, label)}
		// short is the button text beside a longer label; an option with only
		// a short label already shows it as its label.
		if o.Short != "" && o.Label != "" && o.Short != o.Label {
			short := scrubProse(b.ID, o.Short)
			opt.Short = &short
		}
		if o.Description != "" {
			opt.Description = scrubOpt(b.ID, &o.Description)
		}
		// An artifact type this contract version does not list is left out:
		// the server emits only the enum's values.
		if at := landsv1.DecisionArtifactType(o.ArtifactType); at.Valid() {
			opt.ArtifactType = &at
		}
		d.Options = append(d.Options, opt)
	}
	// Evidence (contract 1.0.0-beta.4) is left absent on purpose. The only
	// screenshot links a record carries are the viewer's /image/<key> proxy
	// URLs, and that route is admin-only (beads-viewer route_classes.go), so a
	// land tenant's session gets 403 on every one. Shipping them would hand
	// the tenant broken images that name the fleet's host. It lands with a
	// land-door image route (gd-wRXlwYH2GI).
	if d.Status == landsv1.DecisionStatusResolved {
		if c := b.fieldString("chosen"); c != "" {
			d.Chosen = &c
		}
	}
	return d
}

// escalationStatus: approved once someone approved it; closed without an
// approval is rejected.
func escalationStatus(b *beadRecord) landsv1.EscalationStatus {
	switch {
	case b.fieldString("approved_by") != "":
		return landsv1.EscalationStatusApproved
	case b.Status == model.StatusClosed:
		return landsv1.EscalationStatusRejected
	}
	return landsv1.EscalationStatusOpen
}

func toEscalation(b *beadRecord) landsv1.Escalation {
	e := landsv1.Escalation{
		Id:          b.ID,
		Title:       scrubProse(b.ID, b.Title),
		Reason:      b.fieldString("reason"),
		Status:      escalationStatus(b),
		RequestedBy: displayName(b.fieldString("requesting_agent")),
	}
	if e.Reason == "" {
		e.Reason = b.Description
	}
	e.Reason = scrubProse(b.ID, e.Reason)
	if e.RequestedBy == "" {
		e.RequestedBy = displayName(b.CreatedBy)
	}
	if e.RequestedBy == "" {
		e.RequestedBy = "unknown"
	}
	return e
}

// planState reads fields.plan_state; a plan that never opted into the
// lifecycle reads building while open and complete once closed.
func planState(b *beadRecord) landsv1.PlanState {
	switch s := landsv1.PlanState(b.fieldString("plan_state")); s {
	case landsv1.Draft, landsv1.Ready, landsv1.Building, landsv1.Complete, landsv1.Abandoned:
		return s
	}
	if b.Status == model.StatusClosed {
		return landsv1.Complete
	}
	return landsv1.Building
}

// toPlan projects a plan and its steps; waiting is Map.waiting of the steps.
func toPlan(b *beadRecord, steps []beadRecord, waiting map[string]bool) landsv1.Plan {
	p := landsv1.Plan{Id: b.ID, Title: scrubProse(b.ID, b.Title), State: planState(b), Steps: []landsv1.PlanStep{}, Heraldry: arms(b)}
	for i := range steps {
		s := &steps[i]
		if !isTicketType(s.Type) {
			continue
		}
		p.Steps = append(p.Steps, landsv1.PlanStep{Id: s.ID, Title: scrubProse(s.ID, s.Title), Status: ticketStatus(s.Status, waiting[s.ID]), Heraldry: arms(s)})
	}
	return p
}

// toComment projects a comment on the bead own.
func toComment(own string, c *commentRecord) landsv1.Comment {
	author := c.OperatedByDisplay
	if author == "" {
		author = displayName(c.Author)
	}
	if author == "" {
		author = "unknown"
	}
	return landsv1.Comment{
		Id:       "c" + strconv.FormatInt(c.ID, 36),
		Author:   author,
		Text:     scrubProse(own, c.Text),
		PostedAt: stamp(c.CreatedAt),
	}
}

// memberRole maps a land tier (/v1/me, a member's land_roles) onto a crew
// role.
func memberRole(tier string) landsv1.MemberRole {
	switch tier {
	case "owner":
		return landsv1.MemberRoleOwner
	case "member":
		return landsv1.MemberRoleMember
	case "guest":
		return landsv1.MemberRoleGuest
	}
	return landsv1.MemberRoleOther
}

// displayName turns a principal into something to show: no provider prefix,
// no email domain.
func displayName(s string) string {
	s = strings.TrimSpace(s)
	if p, rest, ok := strings.Cut(s, ":"); ok && p != "" && !strings.ContainsAny(p, "@ ") {
		s = rest
	}
	if local, _, ok := strings.Cut(s, "@"); ok {
		s = local
	}
	return s
}

// plotFrom projects a land's repository: its name is the repository's last
// path element, its id a digest of land and address, so another repository
// in the same land is another plot.
func plotFrom(land string, rec PlotRecord) landsv1.Plot {
	u := strings.TrimSuffix(strings.TrimRight(rec.GitURL, "/"), ".git")
	name := u[strings.LastIndexAny(u, "/:")+1:]
	if name == "" {
		name = land
	}
	branch := rec.DefaultBranch
	if branch == "" {
		// Provisioning always writes one; an older land's agents clone main.
		branch = "main"
	}
	sum := sha256.Sum256([]byte(land + "\x00" + rec.GitURL))
	return landsv1.Plot{Id: "p" + hex.EncodeToString(sum[:10]), Name: name, DefaultBranch: branch}
}

// stamp is the contract's timestamp: UTC, whole seconds.
func stamp(t time.Time) landsv1.Timestamp { return t.UTC().Truncate(time.Second) }

// Cursors are opaque to clients; inside they are an offset into the daemon
// list the page came from.

func encodeCursor(offset int) landsv1.Cursor {
	return base64.RawURLEncoding.EncodeToString([]byte("o" + strconv.Itoa(offset)))
}

// decodeCursor returns 0 for no cursor; ok is false for one the map did not
// mint.
func decodeCursor(c *landsv1.Cursor) (int, bool) {
	if c == nil {
		return 0, true
	}
	raw, err := base64.RawURLEncoding.DecodeString(*c)
	if err != nil || len(raw) < 2 || raw[0] != 'o' {
		return 0, false
	}
	n, err := strconv.Atoi(string(raw[1:]))
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}
