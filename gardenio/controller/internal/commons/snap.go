package commons

import (
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Snapshots (gd-GbF2JS04h5): the farm's Snap button attaches a PNG of the map
// and sends what it framed as the form's context field, which say and reply
// keep as the message bead's snap field. The person's words stay the text; a
// snapshot with none is titled after the place. The context is read here
// field by field and written back, so a key this package does not know never
// reaches the bead, and its view link is stored absolute, as a file link is,
// for the bridge's Slack mirror and an agent nudged by the message.

// Snapshot bounds.
const (
	// snapMaxBytes caps the context field as posted. The farm sends up to
	// snapFramedMax framed entries, each a kind, an id and a title of up to
	// 120 characters (web/commons.js snapContext), so a crowded view is
	// several KB of JSON before parseSnap bounds it; the cap guards the read,
	// not the shape (gd-eB8PbZaReC: at 2 KB a busy view was refused).
	snapMaxBytes = 32 << 10
	// snapFramedMax caps the framed list, as the farm sends it.
	snapFramedMax = 24
	// snapTextMax caps one string of the context, in bytes.
	snapTextMax = 200
)

const fieldSnap = "snap"

// snapContext is the snap field of a message bead.
type snapContext struct {
	Kind    string      `json:"kind"`
	URL     string      `json:"url,omitempty"`
	At      string      `json:"at,omitempty"`
	Z       *int        `json:"z,omitempty"`
	Who     string      `json:"who,omitempty"`
	Night   bool        `json:"night,omitempty"`
	Framed  []snapFrame `json:"framed,omitempty"`
	VP      []int       `json:"vp,omitempty"`
	TakenAt string      `json:"taken_at,omitempty"`
}

// snapFrame is one thing the snapshot framed.
type snapFrame struct {
	Kind  string `json:"kind"`
	ID    string `json:"id"`
	Title string `json:"title,omitempty"`
}

var snapKinds = map[string]bool{"project": true, "plan": true, "bead": true, "biome": true}

// Snap is a message's snapshot as the timeline captions it.
type Snap struct {
	At string // where the camera was, as the address names it; "" over the wild
	// Place is the title of the bead the camera was at (a plan, a phase), from
	// what the snapshot framed, else read with the row's other bead names
	// (withNames); "" leaves the caption on the address.
	Place    string
	Level    int    // 0..3, -1 when unknown
	Who      string // mine | all
	Night    bool
	Projects int
	Plans    int
	Beads    int
	URL      string // the view, absolute
	// On is the message file whose inline image the caption sits under, in
	// its figcaption (gd-fAgk8mVsta); -1 when no file shows inline, and the
	// caption is a line of its own under the files.
	On int
}

// SnapContext and SnapFrame are a message bead's snap field, for a reader
// outside the viewer: the steward renders the snapshot that woke it
// (gd-IMt3KYl1XZ).
type (
	SnapContext = snapContext
	SnapFrame   = snapFrame
)

// ReadSnap reads a message bead's snap field as the timeline does, every
// value bounded; ok only for a farm snapshot.
func ReadSnap(raw string) (SnapContext, bool) {
	c, ok := parseSnap(raw)
	return c, ok && c.Kind == "farm"
}

// SnapPlace names where a snapshot was taken, from its at= value: the
// timeline caption's words ("the wild", "the Yard", gardenio/gd-1).
func SnapPlace(at string) string { return snapPlace(at) }

// snapField reads a posted context into the snap field: "" when there is none
// or the message carries no file to show it with. It answers a bad one itself.
func (h *Handler) snapField(w http.ResponseWriter, r *http.Request, raw string, files int) (string, bool) {
	if strings.TrimSpace(raw) == "" || files == 0 {
		return "", true
	}
	if len(raw) > snapMaxBytes {
		http.Error(w, "That snapshot's context is too large (32 KB at most).", http.StatusRequestEntityTooLarge)
		return "", false
	}
	c, ok := parseSnap(raw)
	if !ok || c.Kind != "farm" {
		http.Error(w, "That snapshot's context is not valid.", http.StatusBadRequest)
		return "", false
	}
	c.URL = h.snapURL(r, c.URL)
	out, _ := json.Marshal(c) // a snapContext always marshals
	return string(out), true
}

// parseSnap reads a snap field, keeping only what this package knows and
// bounding each value. ok is false for anything but a JSON object.
func parseSnap(raw string) (snapContext, bool) {
	var in struct {
		Kind    string          `json:"kind"`
		URL     string          `json:"url"`
		At      string          `json:"at"`
		Z       json.RawMessage `json:"z"`
		Who     string          `json:"who"`
		Night   bool            `json:"night"`
		Framed  []snapFrame     `json:"framed"`
		VP      []int           `json:"vp"`
		TakenAt string          `json:"taken_at"`
	}
	if t := strings.TrimSpace(raw); !strings.HasPrefix(t, "{") || json.Unmarshal([]byte(t), &in) != nil {
		return snapContext{}, false
	}
	c := snapContext{Kind: snapText(in.Kind), URL: snapText(in.URL), At: snapText(in.At), Night: in.Night}
	if z, err := strconv.Atoi(string(in.Z)); err == nil && z >= 0 && z <= 3 {
		c.Z = &z
	}
	if in.Who == "mine" || in.Who == "all" {
		c.Who = in.Who
	}
	for _, f := range in.Framed {
		if len(c.Framed) == snapFramedMax {
			break
		}
		f = snapFrame{Kind: f.Kind, ID: snapText(f.ID), Title: snapText(f.Title)}
		if snapKinds[f.Kind] && f.ID != "" {
			c.Framed = append(c.Framed, f)
		}
	}
	if len(in.VP) == 2 && in.VP[0] > 0 && in.VP[1] > 0 && in.VP[0] <= 1<<15 && in.VP[1] <= 1<<15 {
		c.VP = in.VP
	}
	if t, err := time.Parse(time.RFC3339, in.TakenAt); err == nil {
		c.TakenAt = t.UTC().Format(time.RFC3339)
	}
	return c, true
}

// snapText is one string of the context: no control characters, trimmed and
// at most snapTextMax bytes.
func snapText(s string) string {
	s = strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s))
	return clipBytes(s, snapTextMax)
}

// snapURL is the view link of a context: a path to this viewer's /garden,
// made absolute; anything else is dropped.
func (h *Handler) snapURL(r *http.Request, raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil || strings.HasPrefix(raw, "//") || strings.ContainsAny(raw, `\`) {
		return ""
	}
	if p := u.Path; p != h.BasePath+"/garden" && !strings.HasPrefix(p, h.BasePath+"/garden/") {
		return ""
	}
	u.Fragment = ""
	return requestOrigin(r) + u.String()
}

// snapTitle is the title of a snapshot that carries no words: its place by the
// title the farm framed it with, else by its address.
func snapTitle(raw string) string {
	c, _ := parseSnap(raw)
	if s := snapFrom(raw, nil); s != nil && s.Place != "" {
		return "Snapshot of the farm at " + s.Place
	}
	return "Snapshot of the farm at " + snapPlace(c.At)
}

// snapPlace names where a snapshot was taken, from its at= value.
func snapPlace(at string) string {
	switch at {
	case "":
		return "the wild"
	case "yard":
		return "the Yard"
	}
	if s, err := url.PathUnescape(at); err == nil {
		at = s
	}
	return strings.TrimPrefix(at, "~/")
}

// snapFrom reads a message bead's snap field for the timeline; nil when it has
// none or a malformed one. files are the message's attachments: the first that
// shows inline carries the caption.
func snapFrom(raw string, files []File) *Snap {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	c, ok := parseSnap(raw)
	if !ok || c.Kind != "farm" {
		return nil
	}
	s := &Snap{At: c.At, Level: -1, Who: c.Who, Night: c.Night,
		On: slices.IndexFunc(files, func(f File) bool { return f.Src != "" })}
	if id := snapPlaceID(c.At); id != "" {
		if i := slices.IndexFunc(c.Framed, func(f snapFrame) bool { return f.ID == id && f.Title != "" }); i >= 0 {
			s.Place = c.Framed[i].Title
		}
	}
	if c.Z != nil {
		s.Level = *c.Z
	}
	if u, err := url.Parse(c.URL); err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" {
		s.URL = c.URL
	}
	for _, f := range c.Framed {
		switch f.Kind {
		case "project", "biome":
			s.Projects++
		case "plan":
			s.Plans++
		case "bead":
			s.Beads++
		}
	}
	return s
}

// snapPlaceID is the bead a snapshot's at= value ends on (lab/gd-AbCdEfGhIj
// is the plan gd-AbCdEfGhIj); "" for the wild, the Yard or a land.
func snapPlaceID(at string) string {
	p := snapPlace(at)
	i := strings.LastIndexByte(p, '/')
	if i < 0 {
		return ""
	}
	if id := p[i+1:]; snapBeadID.MatchString(id) && beadLike(id) {
		return id
	}
	return ""
}

// snapBeadID is the shape of a bead id in an address: two letters, a dash and
// any run of letters and digits (a fixture's gd-1 as a fleet's gd-AbCdEfGhIj).
var snapBeadID = regexp.MustCompile(`^[a-z]{2}-[A-Za-z0-9]{1,40}$`)

// snapCaption is the line under a snapshot, before its view link: Farm at
// Staging IdP (z2, Mine, night) · 3 plans, 7 beads framed. The place is the
// title of the bead the camera was at when it is known, else the address
// (gardenio/gd-1).
func snapCaption(s *Snap) string {
	var b strings.Builder
	place := s.Place
	if place == "" {
		place = snapPlace(s.At)
	}
	b.WriteString("Farm at " + place)
	var how []string
	if s.Level >= 0 {
		how = append(how, "z"+strconv.Itoa(s.Level))
	}
	switch s.Who {
	case "mine":
		how = append(how, "Mine")
	case "all":
		how = append(how, "Everyone")
	}
	if s.Night {
		how = append(how, "night")
	}
	if len(how) > 0 {
		b.WriteString(" (" + strings.Join(how, ", ") + ")")
	}
	var framed []string
	for _, n := range []struct {
		n         int
		one, many string
	}{{s.Projects, "project", "projects"}, {s.Plans, "plan", "plans"}, {s.Beads, "bead", "beads"}} {
		if n.n == 1 {
			framed = append(framed, "1 "+n.one)
		} else if n.n > 1 {
			framed = append(framed, strconv.Itoa(n.n)+" "+n.many)
		}
	}
	if len(framed) > 0 {
		b.WriteString(" · " + strings.Join(framed, ", ") + " framed")
	}
	return b.String()
}
