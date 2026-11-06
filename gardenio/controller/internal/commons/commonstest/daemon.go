// Package commonstest is a fake beads daemon for the Commons read model's
// tests: it holds beads and a durable event log, and cuts every read by the
// bearer on the context the way kbeads cuts a scoped principal's reads.
package commonstest

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// Bearers the fixture knows. A bearer in Daemon.Scopes reads only beads
// labelled with one of its projects, plus beads with no project label at all,
// which is kbeads' LabelsOrNoProject cut (so a label-less conversation bead
// reaches every scoped reader, and only the Commons' own land check stops it);
// any other bearer, and a system read, reads everything.
const (
	AdminBearer      = "tok-admin"
	ClaimlayerBearer = "tok-claimlayer"
	NoGroupBearer    = "tok-nogroup"
)

// Daemon is the fake. The zero value is usable; fields may be set directly
// before the first call.
type Daemon struct {
	mu     sync.Mutex
	Beads  map[string]*beadsapi.BeadDetail
	Events []beadsapi.Event
	Scopes map[string][]string
	// Reads counts GetBead calls by bearer ("system" for a system read).
	Reads map[string]int

	// Selves names the principal (oidc:<email>) behind a scoped bearer, for
	// the members-only check on a message write, as kbeads reads it off the
	// token.
	Selves map[string]string
	// Frozen names the frozen lands. The fake models only the one write a
	// freeze lets through: a scoped bearer's message in its land's #support
	// skips the members-only check, as kbeads does (frozenSupportPost).
	Frozen map[string]bool
	// Created and Updated record the writes, in order.
	Created []beadsapi.CreateBeadRequest
	Updated []map[string]any
	// Now stamps created beads; nil is time.Now.
	Now func() time.Time
	// FailWrites, when set, is what every write returns.
	FailWrites error
	// OnCreate, when set, hears every bead created, after it is stored: a
	// world puts it on its event stream, as kbeads does.
	OnCreate func(*beadsapi.BeadDetail)

	// Roster is what GetAgentRoster answers: the kbeads presence tracker's
	// live agents.
	Roster []beadsapi.RosterActor
	// Cursors is each bearer's read_cursors, as kbeads keeps them on the
	// bearer's own user bead. A request with no bearer is refused (403), as
	// kbeads refuses a caller that is not a person.
	Cursors map[string]map[string]string
	// Members is each land's people (oidc:<email> and name), as kbeads lists
	// them from the user beads' allowed_projects.
	Members map[string][]beadsapi.LandMember
	// Comments is each bead's comments, for the reads a decision card's
	// evidence makes (commons evidence.go). A bead with none answers nil.
	Comments map[string][]beadsapi.Comment
	// Deps is the dependency edges: BeadID depends on DependsOnID.
	Deps []beadsapi.Dependency
	// Batches counts the batch reads (BatchGetBeads, BatchGetDependencies).
	Batches int
	nextID  int
}

// CreateBead stores a new bead with an id of its own and logs its created
// event. A scoped bearer may only create in its own projects, and a message in
// a members-only channel only when its Selves principal is a member, as kbeads
// checks (channelPostDenial).
func (d *Daemon) CreateBead(ctx context.Context, req beadsapi.CreateBeadRequest) (string, error) {
	d.mu.Lock()
	if d.FailWrites != nil {
		d.mu.Unlock()
		return "", d.FailWrites
	}
	if !d.visible(ctx, req.Labels) {
		d.mu.Unlock()
		return "", &beadsapi.APIError{StatusCode: http.StatusForbidden, Message: "not authorized to create beads in this project"}
	}
	var fields map[string]string
	_ = json.Unmarshal(req.Fields, &fields)
	bearer := beadsapi.UserBearerFromContext(ctx)
	if _, scoped := d.Scopes[bearer]; scoped && req.Type == "message" {
		ch := d.Beads[fields["channel_id"]]
		support := ch != nil && ch.Fields["name"] == "support" && d.Frozen[landOf(ch.Labels)]
		if ch != nil && !support && (ch.Fields["channel_kind"] == "user" || ch.Fields["channel_kind"] == "dm") {
			var members []string
			_ = json.Unmarshal([]byte(ch.Fields["members"]), &members)
			if !slices.Contains(members, d.Selves[bearer]) {
				d.mu.Unlock()
				return "", &beadsapi.APIError{StatusCode: http.StatusForbidden, Message: "not a member of #" + ch.Fields["name"] + ": join the channel to post in it"}
			}
		}
	}
	// A notification is to a member of its land, as kbeads checks
	// (validateNotificationCreate).
	if req.Type == "notification" && !d.isMember(landOf(req.Labels), fields["to"]) {
		d.mu.Unlock()
		return "", &beadsapi.APIError{StatusCode: http.StatusBadRequest, Message: "to is not a member of this land"}
	}
	d.nextID++
	now := time.Now()
	if d.Now != nil {
		now = d.Now()
	}
	status := req.Status
	if status == "" {
		status = "open"
	}
	b := &beadsapi.BeadDetail{
		ID: "cm-new" + strconv.Itoa(d.nextID), Type: req.Type, Title: req.Title, Status: status,
		Labels: req.Labels, Fields: fields, CreatedBy: req.CreatedBy, CreatedAt: now,
	}
	if b.Fields == nil {
		b.Fields = map[string]string{}
	}
	d.Created = append(d.Created, req)
	d.mu.Unlock()
	d.Put(b)
	d.Log("beads.bead.created", b, now)
	if d.OnCreate != nil {
		d.OnCreate(b)
	}
	return b.ID, nil
}

// UpdateBeadFieldsTyped merges fields into the bead, a list as its JSON text
// (how the daemon returns fields.members), and logs its updated event.
func (d *Daemon) UpdateBeadFieldsTyped(ctx context.Context, id string, fields map[string]any) error {
	d.mu.Lock()
	if d.FailWrites != nil {
		d.mu.Unlock()
		return d.FailWrites
	}
	b, ok := d.Beads[id]
	if !ok {
		d.mu.Unlock()
		return &beadsapi.APIError{StatusCode: http.StatusNotFound, Message: "bead not found"}
	}
	if !d.visible(ctx, b.Labels) {
		d.mu.Unlock()
		return &beadsapi.APIError{StatusCode: http.StatusForbidden, Message: "not authorized"}
	}
	for k, v := range fields {
		if s, ok := v.(string); ok {
			b.Fields[k] = s
			continue
		}
		raw, _ := json.Marshal(v)
		b.Fields[k] = string(raw)
	}
	d.Updated = append(d.Updated, fields)
	at := b.CreatedAt
	if d.Now != nil {
		at = d.Now()
	}
	d.mu.Unlock()
	d.Log("beads.bead.updated", b, at)
	return nil
}

// GetAgentRoster returns Roster, without the agents idle longer than the
// threshold, as kbeads answers.
func (d *Daemon) GetAgentRoster(_ context.Context, staleThresholdSecs int) (*beadsapi.AgentRosterResponse, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := &beadsapi.AgentRosterResponse{Actors: []beadsapi.RosterActor{}}
	for _, a := range d.Roster {
		if staleThresholdSecs > 0 && a.IdleSecs > float64(staleThresholdSecs) {
			continue
		}
		out.Actors = append(out.Actors, a)
	}
	return out, nil
}

// Put stores b.
func (d *Daemon) Put(b *beadsapi.BeadDetail) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.Beads == nil {
		d.Beads = map[string]*beadsapi.BeadDetail{}
	}
	d.Beads[b.ID] = b
}

// Link adds the edge "bead depends on on", of type typ.
func (d *Daemon) Link(bead, on, typ string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.Deps = append(d.Deps, beadsapi.Dependency{BeadID: bead, DependsOnID: on, Type: typ})
}

// BatchGetBeads returns the ids the bearer may read; the rest are left out,
// as kbeads leaves them.
func (d *Daemon) BatchGetBeads(ctx context.Context, ids []string) (map[string]*beadsapi.BeadDetail, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.Batches++
	out := map[string]*beadsapi.BeadDetail{}
	for _, id := range ids {
		if b, ok := d.Beads[id]; ok && d.visible(ctx, b.Labels) {
			cp := *b
			out[id] = &cp
		}
	}
	return out, nil
}

// BatchGetDependencies returns each id's edges, forward (id depends on X) or
// reverse (X depends on id), for the ids the bearer may read.
func (d *Daemon) BatchGetDependencies(ctx context.Context, ids []string, direction string) (map[string][]beadsapi.Dependency, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.Batches++
	out := map[string][]beadsapi.Dependency{}
	for _, id := range ids {
		if b, ok := d.Beads[id]; !ok || !d.visible(ctx, b.Labels) {
			continue
		}
		for _, e := range d.Deps {
			if (direction == "reverse" && e.DependsOnID == id) || (direction != "reverse" && e.BeadID == id) {
				out[id] = append(out[id], e)
			}
		}
	}
	return out, nil
}

// Forget removes every bead match holds for and returns how many; their
// events stay in the log, as a deleted bead's do in kbeads.
func (d *Daemon) Forget(match func(*beadsapi.BeadDetail) bool) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	for id, b := range d.Beads {
		if match(b) {
			delete(d.Beads, id)
			n++
		}
	}
	return n
}

// Log appends a bead event for b to the durable log, with the {"bead": …}
// payload the daemon writes.
func (d *Daemon) Log(topic string, b *beadsapi.BeadDetail, at time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.Events = append(d.Events, beadsapi.Event{
		ID: int64(len(d.Events) + 1), Topic: topic, BeadID: b.ID, Payload: Payload(b), CreatedAt: at,
	})
}

// Payload is the daemon's bead event payload for b.
func Payload(b *beadsapi.BeadDetail) json.RawMessage {
	data, _ := json.Marshal(map[string]any{"bead": map[string]any{
		"id": b.ID, "type": b.Type, "title": b.Title, "status": b.Status, "labels": b.Labels, "fields": b.Fields,
	}})
	return data
}

func (d *Daemon) visible(ctx context.Context, labels []string) bool {
	if beadsapi.IsSystemRead(ctx) {
		return true
	}
	projects, scoped := d.Scopes[beadsapi.UserBearerFromContext(ctx)]
	if !scoped {
		return true
	}
	labelled := false
	for _, l := range labels {
		if p, ok := strings.CutPrefix(l, "project:"); ok {
			labelled = true
			if slices.Contains(projects, p) {
				return true
			}
		}
	}
	return !labelled
}

// GetBead returns the bead, 404 when missing and 403 when outside the bearer's
// projects, as kbeads answers.
func (d *Daemon) GetBead(ctx context.Context, id string) (*beadsapi.BeadDetail, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	who := beadsapi.UserBearerFromContext(ctx)
	if beadsapi.IsSystemRead(ctx) {
		who = "system"
	}
	if d.Reads == nil {
		d.Reads = map[string]int{}
	}
	d.Reads[who]++
	b, ok := d.Beads[id]
	if !ok {
		return nil, &beadsapi.APIError{StatusCode: http.StatusNotFound, Message: "bead not found"}
	}
	if !d.visible(ctx, b.Labels) {
		return nil, &beadsapi.APIError{StatusCode: http.StatusForbidden, Message: "not authorized to read beads in this project"}
	}
	cp := *b
	return &cp, nil
}

// GetComments returns the bead's comments, cut by the bearer like GetBead.
func (d *Daemon) GetComments(ctx context.Context, id string) ([]beadsapi.Comment, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if b, ok := d.Beads[id]; ok && !d.visible(ctx, b.Labels) {
		return nil, &beadsapi.APIError{StatusCode: http.StatusForbidden, Message: "not authorized to read beads in this project"}
	}
	return slices.Clone(d.Comments[id]), nil
}

// ListBeadsFiltered applies Types, ExcludeTypes, Statuses, Labels (all), Fields (equality), FieldsIn,
// Sort (created_at, either way, or -updated_at) and Limit, then the bearer cut.
func (d *Daemon) ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []*beadsapi.BeadDetail
	for _, b := range d.Beads {
		if (len(q.Types) > 0 && !slices.Contains(q.Types, b.Type)) || slices.Contains(q.ExcludeTypes, b.Type) {
			continue
		}
		if len(q.Statuses) > 0 && !slices.Contains(q.Statuses, b.Status) {
			continue
		}
		if !hasAll(b.Labels, q.Labels) || !fieldsMatch(b.Fields, q.Fields) || !fieldsIn(b.Fields, q.FieldsIn) || !d.visible(ctx, b.Labels) {
			continue
		}
		// kbeads compares at the second the query carries, inclusively.
		if !q.CreatedAfter.IsZero() && b.CreatedAt.Before(q.CreatedAfter.Truncate(time.Second)) {
			continue
		}
		if !q.CreatedBefore.IsZero() && b.CreatedAt.After(q.CreatedBefore) {
			continue
		}
		cp := *b
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool {
		if q.Sort == "-updated_at" && !out[i].UpdatedAt.Equal(out[j].UpdatedAt) {
			return out[i].UpdatedAt.After(out[j].UpdatedAt)
		}
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			if q.Sort == "-created_at" {
				return out[i].CreatedAt.After(out[j].CreatedAt)
			}
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return &beadsapi.ListBeadsResult{Beads: out, Total: len(out)}, nil
}

// ListGlobalEvents returns the log at or after Since, oldest first, from
// Cursor (exclusive), Limit at a time, cut by the bearer on the payload's
// labels.
func (d *Daemon) ListGlobalEvents(ctx context.Context, req beadsapi.ListGlobalEventsRequest) (*beadsapi.ListGlobalEventsResponse, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []beadsapi.Event
	more := false
	for _, e := range d.Events {
		if e.ID <= req.Cursor || e.CreatedAt.Before(req.Since) {
			continue
		}
		var p struct {
			Bead struct {
				Labels []string `json:"labels"`
			} `json:"bead"`
		}
		_ = json.Unmarshal(e.Payload, &p)
		if !d.visible(ctx, p.Bead.Labels) {
			continue
		}
		if req.Limit > 0 && len(out) == req.Limit {
			more = true
			break
		}
		out = append(out, e)
	}
	resp := &beadsapi.ListGlobalEventsResponse{Events: out, HasMore: more}
	if more {
		resp.NextCursor = strconv.FormatInt(out[len(out)-1].ID, 10)
	}
	return resp, nil
}

// ReadCursors returns the bearer's read positions.
func (d *Daemon) ReadCursors(ctx context.Context) (map[string]string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	who := beadsapi.UserBearerFromContext(ctx)
	if who == "" {
		return nil, &beadsapi.APIError{StatusCode: http.StatusForbidden, Message: "read cursors need the person's own OIDC token"}
	}
	return maps.Clone(d.Cursors[who]), nil
}

// SetReadCursors lays cursors over the bearer's read positions.
func (d *Daemon) SetReadCursors(ctx context.Context, cursors map[string]string) (map[string]string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	who := beadsapi.UserBearerFromContext(ctx)
	if who == "" {
		return nil, &beadsapi.APIError{StatusCode: http.StatusForbidden, Message: "read cursors need the person's own OIDC token"}
	}
	if d.Cursors == nil {
		d.Cursors = map[string]map[string]string{}
	}
	if d.Cursors[who] == nil {
		d.Cursors[who] = map[string]string{}
	}
	maps.Copy(d.Cursors[who], cursors)
	return maps.Clone(d.Cursors[who]), nil
}

// LandMembers returns land's people, and 404 for a land outside a scoped
// bearer's projects, as kbeads answers.
func (d *Daemon) LandMembers(ctx context.Context, land string) ([]beadsapi.LandMember, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if projects, scoped := d.Scopes[beadsapi.UserBearerFromContext(ctx)]; scoped && !slices.Contains(projects, land) {
		return nil, &beadsapi.APIError{StatusCode: http.StatusNotFound, Message: "no such land"}
	}
	return slices.Clone(d.Members[land]), nil
}

func (d *Daemon) isMember(land, principal string) bool {
	for _, m := range d.Members[land] {
		if m.Principal == principal {
			return true
		}
	}
	return false
}

// landOf is the one project of labels, "" for none or several.
func landOf(labels []string) string {
	land := ""
	for _, l := range labels {
		if p, ok := strings.CutPrefix(l, "project:"); ok {
			if land != "" {
				return ""
			}
			land = p
		}
	}
	return land
}

func hasAll(have, want []string) bool {
	for _, w := range want {
		if !slices.Contains(have, w) {
			return false
		}
	}
	return true
}

func fieldsIn(have map[string]string, want map[string][]string) bool {
	for k, vs := range want {
		if len(vs) > 0 && !slices.Contains(vs, have[k]) {
			return false
		}
	}
	return true
}

func fieldsMatch(have, want map[string]string) bool {
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}
