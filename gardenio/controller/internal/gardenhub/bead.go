package gardenhub

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"
	"gardenio/controller/internal/decisionview"
	"gardenio/controller/internal/kin"
)

// The in-garden bead view (gd-BdhucNqv0i, Homestead World phase 1c): every bead the garden shows opens in a panel
// inside the map window (web/workbench.js, deep link /garden?bead=<id>) instead of the beads-viewer bead page. The snapshot
// and the farm document carry only what the map draws, so the panel's detail (description, fields, comments, parents
// and children) comes from GET /garden/bead/{id}. Unlike the rest of /garden this reads the daemon on the request
// path: it is one bead, on a click, and it must be current. The request context carries the viewer's own bearer, so
// the daemon cuts what it returns exactly as it does for the bead page.

// BeadReader is the slice of the daemon client the bead view reads.
type BeadReader interface {
	GetBead(ctx context.Context, id string) (*beadsapi.BeadDetail, error)
	GetComments(ctx context.Context, id string) ([]beadsapi.Comment, error)
	GetDependencies(ctx context.Context, id string) ([]beadsapi.Dependency, error)
	GetReverseDependencies(ctx context.Context, id string) ([]beadsapi.Dependency, error)
}

const (
	beadViewTimeout = 8 * time.Second
	beadDescMax     = 20000 // runes of description (and of notes) sent; the panel scrolls
	beadFieldMax    = 400   // runes per field value
	beadFieldsMax   = 60    // fields sent, in key order
	beadCommentMax  = 4000  // runes per comment
	beadCommentsMax = 40    // newest comments sent
	beadCommentsAll = 400   // newest comments sent when the panel asks for older ones (?comments=N)
	beadRelMax      = 60    // related beads per bead, all kinds together
	beadStepBodyMax = 1200  // runes of a step's description on a ready or asked plan's child rows
	beadHydrateConc = 8     // related-bead title lookups in flight
)

type beadRel struct {
	ID    string `json:"id"`
	Title string `json:"title,omitempty"`
	// ShortTitle is the bead's short title, trimmed, when it has one: what the panel shows for the relation
	// (gd-QTukIA8Nvv); Title stays for the tooltip.
	ShortTitle string `json:"short_title,omitempty"`
	Type       string `json:"type,omitempty"`
	Status     string `json:"status,omitempty"`
	Rel        string `json:"rel,omitempty"` // the dependency type, for the "related" list
	// Project is the other bead's project label, so the panel can say a
	// relation crosses into another project (gd-CNhEh30Ykf).
	Project string `json:"project,omitempty"`
	// Body is a child step's description, cut at beadStepBodyMax runes, on a plan that is ready or asked (stepBodies), so
	// the panel reads the whole plan from one request.
	Body string `json:"body,omitempty"`
}

type beadField struct {
	K string `json:"k"`
	V string `json:"v"`
}

type beadComment struct {
	Author string `json:"author"`
	Text   string `json:"text"`
	At     int64  `json:"at"`
}

type beadDoc struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Type        string   `json:"type"`
	Status      string   `json:"status"`
	Priority    int      `json:"priority"`
	Assignee    string   `json:"assignee,omitempty"`
	Owner       string   `json:"owner,omitempty"`
	Project     string   `json:"project,omitempty"`
	Labels      []string `json:"labels,omitempty"`
	Description string   `json:"description,omitempty"`
	// DescriptionClipped says Description was cut to beadDescMax: the panel then edits the description on the bead page,
	// never from the cut copy (saving it would drop the rest).
	DescriptionClipped bool `json:"description_clipped,omitempty"`
	// DescriptionIsContext says Description is a decision's context, which its face already shows as its body: the panel
	// skips the Description section, but the Edit form still loads the text (gd-OSI1uK35Wa).
	DescriptionIsContext bool          `json:"description_is_context,omitempty"`
	Notes                string        `json:"notes,omitempty"`
	CreatedBy            string        `json:"created_by,omitempty"`
	OperatedBy           string        `json:"operated_by,omitempty"`
	Created              int64         `json:"created,omitempty"`
	Updated              int64         `json:"updated,omitempty"`
	Closed               string        `json:"closed,omitempty"`
	Fields               []beadField   `json:"fields,omitempty"`
	FieldsMore           int           `json:"fields_more,omitempty"`
	Parents              []beadRel     `json:"parents,omitempty"`
	Children             []beadRel     `json:"children,omitempty"`
	BlockedBy            []beadRel     `json:"blocked_by,omitempty"`
	Blocks               []beadRel     `json:"blocks,omitempty"`
	Related              []beadRel     `json:"related,omitempty"`
	RelMore              int           `json:"rel_more,omitempty"`
	Comments             []beadComment `json:"comments,omitempty"`
	CommentsAll          int           `json:"comments_total"`
	// Face is the bead's typed face (bead_face.go, WB-1), keyed by kind; absent on a type that keeps the generic fields table.
	Face *beadFace `json:"face,omitempty"`
	// Arms are the bead's arms (bead_arms.go, heraldry step 3.2): the run, its count and its plain-words name; absent when
	// it bears none or its type may not bear them. ArmsOK says its type may bear arms (R6), so the panel offers the
	// Heraldry section and Change arms.
	Arms *beadArms `json:"arms,omitempty"`
	// LineArms are the arms of the bead's line when it bears none itself (line_arms.go, gd-PUTYxm5BtX): its nearest
	// armed ancestor's, drawn before the title; LineFrom is the bead that bears them, named in the picture's tooltip.
	LineArms *beadArms `json:"line_arms,omitempty"`
	LineFrom *beadRel  `json:"line_from,omitempty"`
	ArmsOK   bool      `json:"arms_ok,omitempty"`
	// Harvest is what a closed work bead yielded (bead_harvest.go, harvest spec §6); absent on anything else.
	Harvest *beadHarvest `json:"harvest,omitempty"`
	// Mentions titles the bead ids the free text names (bead_mentions.go), id -> title; an id the daemon did not return is absent.
	Mentions map[string]string `json:"mentions,omitempty"`
	// Unseen are the ids the free text names that the daemon did not return (gone, or not visible to this viewer), so the
	// panel says so beside the id instead of a bare link.
	Unseen []string `json:"unseen,omitempty"`
	// Kin are the beads bound to the bead's thread by thread_id or parent-child (kin.Doc): its conversation, the work
	// it gave rise to, its agents and sessions, and the head when this bead is not it. Absent for a bead in no thread.
	Kin *kin.Doc `json:"kin,omitempty"`
	// Spawn says the viewer may start an agent on this bead from the panel (Handler.MaySpawn for its project, Lands WB-12).
	Spawn bool `json:"spawn,omitempty"`
	// Partial names the parts the daemon did not answer (comments, deps); the panel says so instead of showing none.
	Partial []string `json:"partial,omitempty"`
}

// beadView serves GET /garden/bead/{id}: one bead as the in-garden panel needs it.
func (h *Handler) beadView(w http.ResponseWriter, r *http.Request) {
	if h.Beads == nil {
		http.Error(w, "bead view not enabled", http.StatusServiceUnavailable)
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" || len(id) > 200 {
		http.Error(w, "bad bead id", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), beadViewTimeout)
	defer cancel()
	if after, err := strconv.Atoi(r.URL.Query().Get("after")); err == nil && after >= 0 {
		h.beadComments(ctx, w, id, after)
		return
	}
	n := beadCommentsMax
	if v, err := strconv.Atoi(r.URL.Query().Get("comments")); err == nil && v > n { // "older" in the panel's comments
		n = min(v, beadCommentsAll)
	}
	doc, err := readBeadN(ctx, h.Beads, id, n, h.Evidence)
	if err != nil {
		beadReadFailed(w, err)
		return
	}
	doc.Spawn = h.MaySpawn != nil && !h.readOnly(r) && h.MaySpawn(r, doc.Project)
	writeTagged(w, r, "kbeads", doc)
}

func beadReadFailed(w http.ResponseWriter, err error) {
	var ae *beadsapi.APIError
	switch {
	case errors.As(err, &ae) && (ae.StatusCode == http.StatusNotFound || ae.StatusCode == http.StatusForbidden):
		http.Error(w, "no such bead", http.StatusNotFound) // a bead the viewer may not see is not there, as on the bead page
	default:
		http.Error(w, "the beads daemon did not answer", http.StatusBadGateway)
	}
}

// beadCommentsDoc answers GET /garden/bead/{id}?after=N: the bead's comments after the first N the panel holds, for a move
// that only added comments (live.go), and the titles of the ids they name. When the bead has fewer than N (some were
// deleted) or more than beadCommentsMax new ones, Comments is empty and the panel fetches the whole bead.
type beadCommentsDoc struct {
	ID       string            `json:"id"`
	Comments []beadComment     `json:"comments"`
	Total    int               `json:"comments_total"`
	Mentions map[string]string `json:"mentions,omitempty"`
	Unseen   []string          `json:"unseen,omitempty"`
}

// beadComments serves the ?after= read: the bead itself, so a bead the viewer may not see is a 404 as for the whole
// document, and its comments, read together.
func (h *Handler) beadComments(ctx context.Context, w http.ResponseWriter, id string, after int) {
	var (
		b        *beadsapi.BeadDetail
		comments []beadsapi.Comment
		bErr     error
		cErr     error
		wg       sync.WaitGroup
	)
	wg.Add(2)
	go func() { defer wg.Done(); b, bErr = h.Beads.GetBead(ctx, id) }()
	go func() { defer wg.Done(); comments, cErr = h.Beads.GetComments(ctx, id) }()
	wg.Wait()
	if bErr == nil && b == nil {
		bErr = &beadsapi.APIError{StatusCode: http.StatusNotFound, Message: "no such bead"}
	}
	if bErr == nil {
		bErr = cErr
	}
	if bErr != nil {
		beadReadFailed(w, bErr)
		return
	}
	doc := beadCommentsDoc{ID: id, Comments: []beadComment{}, Total: len(comments)}
	if fresh := len(comments) - after; fresh > 0 && fresh <= beadCommentsMax {
		doc.Comments = commentRows(comments[after:])
		ment := mentions(&beadsapi.BeadDetail{ID: id}, comments[after:], harvestCrews{})
		all := make([]*beadRel, len(ment))
		for i := range ment {
			all[i] = &ment[i]
		}
		doc.Mentions, doc.Unseen = mentionTitles(ment, hydrate(ctx, h.Beads, all))
	}
	writeDoc(w, "kbeads", doc)
}

// commentRows is the comments as the panel shows them.
func commentRows(comments []beadsapi.Comment) []beadComment {
	var out []beadComment
	for _, c := range comments {
		var at int64
		if !c.CreatedAt.IsZero() {
			at = c.CreatedAt.Unix()
		}
		out = append(out, beadComment{Author: firstNonEmpty(c.OperatedByDisplay, c.Author, c.OperatedBy), Text: clipKeep(c.Text, beadCommentMax), At: at})
	}
	return out
}

// readBeadN assembles the panel's document: the bead, then its newest maxComments comments and both dependency
// directions in parallel, then the titles of the related beads (bounded). Only the bead itself is required; the rest
// degrades to Partial. A decision's evidence is read through ev (nil reads it every time).
func readBeadN(ctx context.Context, src BeadReader, id string, maxComments int, ev *decisionview.EvidenceCache) (*beadDoc, error) {
	b, err := src.GetBead(ctx, id)
	if err != nil {
		return nil, err
	}
	if b == nil {
		return nil, &beadsapi.APIError{StatusCode: http.StatusNotFound, Message: "no such bead"}
	}
	d := &beadDoc{
		ID: b.ID, Title: b.Title, Type: b.Type, Status: b.Status, Priority: b.Priority, Assignee: b.Assignee, Owner: b.Owner,
		Project: projectOf(b), Labels: b.Labels, Description: clipKeep(b.Description, beadDescMax), Notes: clipKeep(b.Notes, beadDescMax),
		CreatedBy: b.CreatedBy, OperatedBy: firstNonEmpty(b.OperatedByDisplay, b.OperatedBy), Closed: b.ClosedAt,
		DescriptionClipped: len([]rune(strings.TrimSpace(b.Description))) > beadDescMax,
		ArmsOK:             armsBearerTypes[b.Type],
	}
	if _, src := decisionview.Context(b); b.Type == "decision" && src == "description" {
		d.DescriptionIsContext = true // its face's body: shown once, yet kept for the Edit form (beadwrite.js)
	}
	if d.ArmsOK { // a task bearing arms (an older row, or one the daemon took before R6) shows none (heraldry step 3.4)
		d.Arms = armsOf(b.Heraldry)
	}
	if !b.CreatedAt.IsZero() {
		d.Created = b.CreatedAt.Unix()
	}
	if !b.UpdatedAt.IsZero() {
		d.Updated = b.UpdatedAt.Unix()
	}
	keys := make([]string, 0, len(b.Fields))
	for k := range b.Fields {
		if !faceShows(b, k) && strings.TrimSpace(b.Fields[k]) != "" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for i, k := range keys {
		if i >= beadFieldsMax {
			d.FieldsMore = len(keys) - beadFieldsMax
			break
		}
		d.Fields = append(d.Fields, beadField{K: k, V: clipKeep(b.Fields[k], beadFieldMax)})
	}

	var (
		wg               sync.WaitGroup
		comments         []beadsapi.Comment
		fwd, rev         []beadsapi.Dependency
		cErr, fErr, rErr error
		crews            harvestCrews
		harvest          = isHarvestable(b)
		park             *bridge.ReconcileDormancy
		planG            *beadsapi.BeadDetail
		evidence         []decisionview.Image
		kinDoc           *kin.Doc
		kinFailed        bool
	)
	if b.Type == "decision" { // the screenshots on the beads it is about, under its context (gd-Pi7C83262N)
		wg.Add(1)
		go func() { defer wg.Done(); evidence = ev.Load(ctx, src, b, 0) }()
	}
	wg.Add(4)
	go func() { defer wg.Done(); comments, cErr = src.GetComments(ctx, id) }()
	go func() { defer wg.Done(); fwd, fErr = src.GetDependencies(ctx, id) }()
	go func() { defer wg.Done(); rev, rErr = src.GetReverseDependencies(ctx, id) }()
	// A gardener loop's live park, or a plan's gardener and its park (bead_face.go facePark), nil for anything else.
	go func() { defer wg.Done(); planG, park = facePark(ctx, src, b) }()
	if harvest { // the crews that claimed the step, for its cost and wrap-up
		wg.Add(1)
		go func() { defer wg.Done(); crews = readCrews(ctx, src, id) }()
	}
	wg.Add(1)
	go func() { defer wg.Done(); kinDoc, kinFailed = readKin(ctx, src, b) }()
	wg.Wait()

	if cErr != nil {
		d.Partial = append(d.Partial, "comments")
	}
	d.CommentsAll = len(comments)
	if len(comments) > maxComments {
		comments = comments[len(comments)-maxComments:]
	}
	d.Comments = commentRows(comments)
	if fErr != nil || rErr != nil {
		d.Partial = append(d.Partial, "links")
	}
	if d.Kin = kinDoc; kinFailed {
		d.Partial = append(d.Partial, "kin")
	}
	if d.Arms == nil && fErr == nil {
		d.LineArms, d.LineFrom = beadLineArms(ctx, src, b, fwd)
	}

	// Forward rows read "id depends on X": a parent-child edge names our parent, a blocks edge a bead we wait on.
	// Reverse rows read "X depends on id": our children, and the beads we hold up.
	n, seen := 0, map[string]bool{}
	add := func(list *[]beadRel, other, rel string) {
		key := rel + "\x00" + other
		if other == "" || other == id || seen[key] {
			return
		}
		seen[key] = true
		if n >= beadRelMax {
			d.RelMore++
			return
		}
		n++
		*list = append(*list, beadRel{ID: other, Rel: rel})
	}
	for _, dp := range fwd {
		switch dp.Type {
		case "parent-child":
			add(&d.Parents, dp.DependsOnID, "")
		case "blocks":
			add(&d.BlockedBy, dp.DependsOnID, "")
		default:
			add(&d.Related, dp.DependsOnID, dp.Type)
		}
	}
	for _, dp := range rev {
		switch dp.Type {
		case "parent-child":
			add(&d.Children, dp.BeadID, "")
		case "blocks":
			add(&d.Blocks, dp.BeadID, "")
		default:
			add(&d.Related, dp.BeadID, dp.Type)
		}
	}
	face, all := buildFace(b) // the beads the face names (who asked, the task, the last run) are titled with the rest
	// A plan's gardener was read whole by the list that found it, so it is not hydrated again.
	if face != nil && face.Plan != nil && planG != nil {
		face.Plan.Gardener = &beadRel{ID: planG.ID, Title: planG.Title, Type: planG.Type, Status: planG.Status, Project: projectOf(planG)}
		face.Plan.GardenerState = loopState(planG)
	}
	if face != nil && park != nil && park.LiveBlocker != id {
		dormant := &beadRel{ID: strings.TrimSpace(park.LiveBlocker)}
		switch {
		case face.Gardener != nil:
			face.Gardener.Dormant, face.Gardener.DormantKind = dormant, park.Kind
			all = append(all, dormant)
		case face.Plan != nil && face.Plan.Gardener != nil:
			face.Plan.Dormant, face.Plan.DormantKind = dormant, park.Kind
			all = append(all, dormant)
		}
	}
	for _, l := range [][]beadRel{d.Parents, d.Children, d.BlockedBy, d.Blocks, d.Related} {
		for i := range l {
			all = append(all, &l[i])
		}
	}
	ment := mentions(b, comments, crews)
	for i := range ment {
		all = append(all, &ment[i])
	}
	got := hydrate(ctx, src, all)
	d.Mentions, d.Unseen = mentionTitles(ment, got)
	finishFace(face, d, got)
	if face != nil && face.Decision != nil {
		face.Decision.Evidence = faceEvidenceOf(evidence)
	}
	stepBodies(b, d, got)
	d.Face = face
	if harvest {
		d.Harvest = buildHarvest(b, d.Children, got, crews)
	}
	return d, nil
}

// faceShows reports whether the bead's face shows field k, so the generic fields table leaves it out: a decision's context
// is its body, not a 400-rune row in the fold (gd-OSI1uK35Wa); a release's contents and deployments are its face, not a
// cut JSON row (gd-j3bQqfZSt7); a wish's ask leads its face (gd-zn6iUnPd9Q).
func faceShows(b *beadsapi.BeadDetail, k string) bool {
	switch {
	case b.Type == "decision":
		return k == "context"
	case b.Type == "release":
		return releaseFaceFields[k]
	}
	return wishFields[k] && faceKindOf(b) == "wish"
}

// beadMentioner is a reader of the daemon's reverse mentions and of many beads at once: the messages that name a bead.
type beadMentioner interface {
	GetMentionedBy(ctx context.Context, id string, limit int) ([]beadsapi.BeadReferenceSummary, error)
	BatchGetBeads(ctx context.Context, ids []string) (map[string]*beadsapi.BeadDetail, error)
}

// readKin reads the kin of b's thread (gd-58mkDiWkNq): one list by thread_id and, when b is not the head, the head
// itself, the head's parent-child closure (readUnbound), the messages out of the thread that name b (readMentioned,
// gd-UXDp9GUeMB) and, for an agent, the messages it wrote (kin.AuthoredQuery, gd-GTqYxAem2S), in parallel. It returns
// nil for a bead in no thread that no message names or a reader without the list, and failed when the list did not
// answer. A bead in no thread that messages name has kin of those messages alone (NoThread).
func readKin(ctx context.Context, src BeadReader, b *beadsapi.BeadDetail) (doc *kin.Doc, failed bool) {
	l, ok := src.(beadLister)
	if !ok {
		return nil, false
	}
	var (
		wg       sync.WaitGroup
		named    []*beadsapi.BeadDetail
		authored *beadsapi.ListBeadsResult
		closure  []string
		thread   = kin.ThreadOf(b)
		head     = b
		res      *beadsapi.ListBeadsResult
		listErr  error
		agent    = strings.TrimSpace(b.Title)
	)
	wg.Add(1)
	go func() { defer wg.Done(); named = readMentioned(ctx, src, b.ID) }()
	if b.Type == "agent" && agent != "" {
		wg.Add(1)
		go func() { defer wg.Done(); authored, _ = l.ListBeadsFiltered(ctx, kin.AuthoredQuery(agent)) }() // none on error
	}
	if thread != "" && thread != b.ID {
		wg.Add(1)
		go func() { defer wg.Done(); head, _ = src.GetBead(ctx, thread) }() // a head the viewer may not see is left out
	}
	if thread != "" {
		wg.Add(1)
		go func() { defer wg.Done(); closure = readClosure(ctx, src, thread) }()
		res, listErr = l.ListBeadsFiltered(ctx, kin.Query(thread))
	}
	wg.Wait()
	switch {
	case thread == "" && len(named) == 0 && (authored == nil || len(authored.Beads) == 0):
		return nil, false
	case thread == "":
		doc = &kin.Doc{Conversation: []kin.Line{}, NoThread: true}
	case listErr != nil || res == nil:
		return nil, true
	default:
		doc = kin.Of(b, head, slices.Concat(res.Beads, readUnbound(ctx, src, thread, kin.Unbound(closure, res.Beads))))
	}
	doc.Mentioned(named)
	if authored != nil {
		doc.Authored(agent, authored.Beads)
	}
	return doc, false
}

// kinClosureDepth is how many levels of a thread head's parent-child children readClosure walks.
const kinClosureDepth = 16

// beadClosure is a reader of many beads and their dependencies at once: a thread's parent-child closure.
type beadClosure interface {
	BatchGetDependencies(ctx context.Context, ids []string, direction string) (map[string][]beadsapi.Dependency, error)
	BatchGetBeads(ctx context.Context, ids []string) (map[string]*beadsapi.BeadDetail, error)
}

// readClosure is the ids of head's parent-child descendants, the head left out, nearest first and at most kin.Max of
// them, one batch read a level; what it has when a read fails, none from a reader without the batch reads.
func readClosure(ctx context.Context, src BeadReader, head string) []string {
	r, ok := src.(beadClosure)
	if !ok {
		return nil
	}
	var ids []string
	seen := map[string]bool{head: true}
	for level, depth := []string{head}, 0; len(level) > 0 && depth < kinClosureDepth; depth++ {
		revs, err := r.BatchGetDependencies(ctx, level, "reverse")
		if err != nil {
			return ids
		}
		var next []string
		for _, id := range level {
			for _, d := range revs[id] {
				if d.Type == "parent-child" && d.BeadID != "" && !seen[d.BeadID] && len(ids) < kin.Max {
					seen[d.BeadID] = true
					next, ids = append(next, d.BeadID), append(ids, d.BeadID)
				}
			}
		}
		level = next
	}
	return ids
}

// readUnbound reads ids, the closure members the thread_id list missed (kin.Unbound), in one batch and keeps those
// that name no other thread: a descendant bound to another thread is that thread's kin.
func readUnbound(ctx context.Context, src BeadReader, thread string, ids []string) []*beadsapi.BeadDetail {
	r, ok := src.(beadClosure)
	if !ok || len(ids) == 0 {
		return nil
	}
	got, err := r.BatchGetBeads(ctx, ids)
	if err != nil {
		return nil
	}
	var out []*beadsapi.BeadDetail
	for _, id := range ids {
		if b := got[id]; b != nil && !kin.IsWishHead(b) && (b.Fields["thread_id"] == "" || b.Fields["thread_id"] == thread) {
			out = append(out, b)
		}
	}
	return out
}

// readMentioned is the messages that name id in their text, as the daemon's reverse mentions find them (kin.MentionIDs),
// read in one batch; none from a reader without them, or when either read fails: the conversation stands without them.
func readMentioned(ctx context.Context, src BeadReader, id string) []*beadsapi.BeadDetail {
	m, ok := src.(beadMentioner)
	if !ok {
		return nil
	}
	refs, err := m.GetMentionedBy(ctx, id, kin.MentionMax)
	ids := kin.MentionIDs(id, refs)
	if err != nil || len(ids) == 0 {
		return nil
	}
	got, err := m.BatchGetBeads(ctx, ids)
	if err != nil {
		return nil
	}
	out := make([]*beadsapi.BeadDetail, 0, len(ids))
	for _, i := range ids {
		if b := got[i]; b != nil {
			out = append(out, b)
		}
	}
	return out
}

// stepBodies gives each child row of a plan that is waiting to be approved its step's description: plan_state ready, or
// an open decision it is blocked on (asked, as the steward's "run it?" blocks it; farm_seeds.go). A building or draft
// plan's children keep titles only.
func stepBodies(b *beadsapi.BeadDetail, d *beadDoc, got map[string]*beadsapi.BeadDetail) {
	if b.Type != "epic" || !hasLabel(b, "plan") {
		return
	}
	asked := strings.TrimSpace(b.Fields["plan_state"]) == "ready"
	for _, r := range d.BlockedBy {
		asked = asked || (r.Type == "decision" && r.Status != "" && r.Status != "closed")
	}
	if !asked {
		return
	}
	for i := range d.Children {
		if c := got[d.Children[i].ID]; c != nil {
			d.Children[i].Body = clipKeep(c.Description, beadStepBodyMax)
		}
	}
}

// hydrate fills each related bead's title, type and status, a few lookups at a time, and returns the beads it read by
// id. A bead the daemon does not return (gone, or not visible to this viewer) keeps only its id.
func hydrate(ctx context.Context, src BeadReader, rels []*beadRel) map[string]*beadsapi.BeadDetail {
	ids := map[string][]*beadRel{}
	var order []string
	for _, r := range rels {
		if _, ok := ids[r.ID]; !ok {
			order = append(order, r.ID)
		}
		ids[r.ID] = append(ids[r.ID], r)
	}
	var (
		wg  sync.WaitGroup
		mu  sync.Mutex
		sem = make(chan struct{}, beadHydrateConc)
		got = make(map[string]*beadsapi.BeadDetail, len(order))
	)
	for _, id := range order {
		wg.Add(1)
		sem <- struct{}{}
		go func(id string) {
			defer wg.Done()
			defer func() { <-sem }()
			b, err := src.GetBead(ctx, id)
			if err != nil || b == nil {
				return
			}
			mu.Lock()
			got[id] = b
			for _, r := range ids[id] {
				r.Title, r.ShortTitle, r.Type, r.Status, r.Project = b.Title, strings.TrimSpace(b.ShortTitle), b.Type, b.Status, projectOf(b)
			}
			mu.Unlock()
		}(id)
	}
	wg.Wait()
	return got
}

// clipKeep cuts s to at most n runes, keeping its line breaks (clip collapses whitespace, which a description needs).
func clipKeep(s string, n int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimSpace(string(r[:n-1])) + "…"
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}
