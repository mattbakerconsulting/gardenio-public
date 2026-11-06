package gardenhub

import (
	"fmt"
	"net/http"
	"strconv"

	"gardenio/controller/internal/heraldry"
)

// armsCache holds the World's arms pictures (heraldry spec §6.4, step 4.1):
// one PNG per plan arms, device height and theme, so a farm of a few dozen
// plans at two densities and both themes stays well inside it.
var armsCache = heraldry.NewCache(512)

// arms serves GET /garden/arms.png?arms=<stored>&h=<device px>&theme=day|night:
// the renderer's PNG of a plan's arms, a row of its charges at the height
// web/heraldry.js asks for (the plot gate's 24 CSS px times the screen's
// devicePixelRatio) and as wide as the run needs, drawn once
// and then served from armsCache. The query carries the inputs, not
// heraldry.Key, so the canvas can name a picture without hashing; the Key is
// the ETag, and a day's private cache keeps the browser from asking again
// (the Key holds heraldry.Version, so a new drawing revalidates as new). A
// form the renderer refuses, a height outside its range or another theme is
// a 400; the canvas then leaves the gate bare.
func arms(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	height, err := strconv.Atoi(q.Get("h"))
	if err != nil || height < heraldry.MinHeight || height > heraldry.MaxHeight {
		http.Error(w, fmt.Sprintf("h must be a device height from %d to %d", heraldry.MinHeight, heraldry.MaxHeight), http.StatusBadRequest)
		return
	}
	th := heraldry.Day
	switch q.Get("theme") {
	case "", "day":
	case "night":
		th = heraldry.Night
	default:
		http.Error(w, "theme must be day or night", http.StatusBadRequest)
		return
	}
	p, err := armsCache.PNG(q.Get("arms"), height, th)
	if err != nil {
		http.Error(w, "not arms the renderer can draw", http.StatusBadRequest)
		return
	}
	etag := `"` + p.Key + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "private, max-age=86400")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", p.ContentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(p.Bytes)))
	_, _ = w.Write(p.Bytes)
}

// armsName is arms in a stored form in plain words ("arms: fire, compass,
// package", heraldry spec §8),
// for the farm's plan and phase rows: the canvas has no library to name a
// charge with, so the plot's and the stake's hots read this. "" when there
// are none or the renderer could not draw them either.
func armsName(canonical string) string {
	if canonical == "" {
		return ""
	}
	a, err := heraldry.Parse(canonical)
	if err != nil {
		return ""
	}
	return a.PlainName()
}
