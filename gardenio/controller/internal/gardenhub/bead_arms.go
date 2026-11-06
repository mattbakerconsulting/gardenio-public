package gardenhub

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/heraldry"
)

// The bead view's Heraldry section (heraldry plan gd-N6I3Oa7uDg steps 3.2 and 5.5b; spec
// gardenio/docs/superpowers/specs/2026-10-25-heraldry-design.md §5, §7, §8): an epic's arms as the renderer's picture (a
// row of its charges, 64 px high) with the plain-words name beside it, and under them the Change arms control (web/arms.js),
// a correction tool for an operator who dislikes what an agent chose (R7: agents pick arms): one row of three to five
// emoji picked from the library in order. The control saves through the viewer's own POST /b/{id}/heraldry, which the
// daemon checks again (R5, R6, R11).
//
// The page never shows a text emoji (spec §5: no text emoji anywhere in /garden): the picture is the SVG from
// GET /garden/arms.svg, each charge in the library grid and in the row is its Noto PNG from GET /garden/arms/charge/{cp},
// and every name is words. The library and the register carry the emoji only as the values the control posts and compares.
//
//	GET /garden/arms.svg?a=<run>&theme=day|night         the picture, resolution-free (sized by its height, §6.3)
//	GET /garden/arms/charge/{codepoints}                 one charge's artwork, 128 px PNG
//	GET /garden/arms/library                             the charges with their names
//	GET /garden/arms/register?project=<name>             the beads bearing arms in that project, for the live collisions

// beadArms is a bead's arms as the panel shows them: the run in the library's spelling (heraldry.Arms.Canonical, also for a
// bead that stored the six-token form before arms became a run; the picture's input and the control's starting point, never
// printed), how many emoji it holds (the picture's width, heraldry.Width) and its plain-words name (§8), which is the
// picture's alt and the words beside it.
type beadArms struct {
	Canonical string `json:"canonical"`
	N         int    `json:"n"`
	Name      string `json:"name"`
}

// armsOf is the bead's arms, nil when it bears none or bears a form this renderer cannot read (a newer library on the
// daemon): the panel then shows no picture rather than a broken one.
func armsOf(stored string) *beadArms {
	a, err := heraldry.Parse(stored)
	if err != nil {
		return nil
	}
	return &beadArms{Canonical: a.Canonical(), N: len(a.Charges), Name: a.PlainName()}
}

// armsBearerTypes are the bead types that may bear arms (kbeads heraldryTypes, R6: epics, plans and projects first). The
// document says so (beadDoc.ArmsOK) so the panel offers the section and the control only where the daemon would take arms.
var armsBearerTypes = map[string]bool{"epic": true, "project": true}

// armsSVGCache holds the panel's pictures: two themes per arms, for the bead open and the drafts the control previews.
var armsSVGCache = heraldry.NewCache(256)

// armsMaxAge is how long the browser keeps a picture or a charge: the drawing's version is in the ETag (heraldry.Key), and
// the artwork only moves with a new build.
const armsMaxAge = "private, max-age=86400"

// armsSVG serves GET /garden/arms.svg?a=<canonical>&theme=day|night: the renderer's SVG of arms in the canonical form,
// with its charges embedded, so it draws through an <img> (an SVG image loads nothing of its own). A form the renderer
// refuses, or another theme, is a 400.
func armsSVG(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	th := heraldry.Day
	switch q.Get("theme") {
	case "", "day":
	case "night":
		th = heraldry.Night
	default:
		http.Error(w, "theme must be day or night", http.StatusBadRequest)
		return
	}
	p, err := armsSVGCache.SVG(q.Get("a"), th)
	if err != nil {
		http.Error(w, "not arms the renderer can draw", http.StatusBadRequest)
		return
	}
	etag := `"` + p.Key + `"`
	h := w.Header()
	h.Set("ETag", etag)
	h.Set("Cache-Control", armsMaxAge)
	h.Set("X-Content-Type-Options", "nosniff")
	// Opened as a document rather than through <img>, the SVG still runs nothing (cmd/beads-viewer arms.go).
	h.Set("Content-Security-Policy", "default-src 'none'; img-src data:; style-src 'unsafe-inline'")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	h.Set("Content-Type", p.ContentType)
	_, _ = w.Write(p.Bytes)
}

// chargesByCode finds a library charge by its codepoints, the artwork's file name.
var chargesByCode = sync.OnceValue(func() map[string]heraldry.Charge {
	m := make(map[string]heraldry.Charge, len(heraldry.Library()))
	for _, c := range heraldry.Library() {
		m[c.Codepoints] = c
	}
	return m
})

// armsCharge serves GET /garden/arms/charge/{codepoints}: one library charge's artwork, the 128 px Noto PNG, for the
// control's library grid and its row. A name that is no library charge is a 404.
func armsCharge(w http.ResponseWriter, r *http.Request) {
	c, ok := chargesByCode()[r.PathValue("cp")]
	if !ok {
		http.NotFound(w, r)
		return
	}
	png, ok := heraldry.ChargePNG(c)
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", armsMaxAge)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(png)
}

// armsLibraryCharge is one charge in GET /garden/arms/library: its emoji (the value the control posts, never shown), its
// plain-words name and its codepoints (its artwork's URL).
type armsLibraryCharge struct {
	E string `json:"e"`
	N string `json:"n"`
	C string `json:"c"`
}

// armsLibrary is the body of GET /garden/arms/library, built once: the renderer's own library (heraldry.Library, kept in
// step with kbeads' by TestKbeadsParity), so the control offers exactly the charges the picture can draw.
type armsLibrary struct {
	Charges []armsLibraryCharge `json:"charges"`
}

var armsLibraryBody = sync.OnceValues(func() ([]byte, error) {
	var lib armsLibrary
	for _, c := range heraldry.Library() {
		lib.Charges = append(lib.Charges, armsLibraryCharge{E: c.Emoji, N: c.Name, C: c.Codepoints})
	}
	return json.Marshal(lib)
})

// armsLibraryServe serves GET /garden/arms/library: the same for every viewer, so it carries no bead data and the browser
// keeps it for a day (its ETag is a hash of the body).
func armsLibraryServe(w http.ResponseWriter, r *http.Request) {
	body, err := armsLibraryBody()
	if err != nil {
		http.Error(w, "the arms library could not be read", http.StatusInternalServerError)
		return
	}
	sum := sha256.Sum256(body)
	etag := `"` + hex.EncodeToString(sum[:8]) + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", armsMaxAge)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(body)
}

// armsRegisterLimit bounds the register read, as kd heraldry check and the daemon's own check do (kbeads
// heraldryRegisterLimit). With arms on epics only (R6) a project holds tens of arms, not thousands.
const armsRegisterLimit = 5000

const armsRegisterTimeout = 8 * time.Second

// armsBearer is one bead of a project's register: who bears which arms, for the control's live collisions.
type armsBearer struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Arms  string `json:"arms"` // the run (beadArms.Canonical)
	Name  string `json:"name"` // plain words (§8)
}

// armsRegister serves GET /garden/arms/register?project=<name>: the beads bearing arms in the project's register, read
// as the viewer (the request's bearer), so a bead the viewer cannot see is not named. The daemon checks the whole
// register again when the arms are saved, so a collision with a bead hidden from the viewer still refuses the save.
func (h *Handler) armsRegister(w http.ResponseWriter, r *http.Request) {
	project := strings.TrimSpace(r.URL.Query().Get("project"))
	if project == "" || len(project) > 200 {
		http.Error(w, "project is required", http.StatusBadRequest)
		return
	}
	l, ok := h.Beads.(beadLister)
	if h.Beads == nil || !ok {
		http.Error(w, "the arms register is not enabled", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), armsRegisterTimeout)
	defer cancel()
	out, err := readArmsRegister(ctx, l, project)
	if err != nil {
		http.Error(w, "the beads daemon did not answer", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{"project": project, "bearers": out})
}

// readArmsRegister lists project's register as kd heraldry check does: the register filter, and the project label too
// so a daemon that predates heraldry_project still reads only the project's beads.
func readArmsRegister(ctx context.Context, l beadLister, project string) ([]armsBearer, error) {
	res, err := l.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		HeraldryProject: project, Labels: []string{"project:" + project}, Limit: armsRegisterLimit,
	}.NoFields())
	if err != nil {
		return nil, err
	}
	out := []armsBearer{}
	if res == nil {
		return out, nil
	}
	for _, b := range res.Beads {
		if b == nil {
			continue
		}
		a := armsOf(b.Heraldry)
		if a == nil {
			continue
		}
		out = append(out, armsBearer{ID: b.ID, Title: firstNonEmpty(b.ShortTitle, b.Title), Arms: a.Canonical, Name: a.Name})
	}
	return out, nil
}
