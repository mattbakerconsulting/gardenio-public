package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// POST /b/{id}/heraldry (heraldry step 3.2): the Workbench's Change arms sends
// the arms, or "none", to the daemon as the bead's heraldry, and a JSON caller
// hears the daemon's refusal in its words.
func TestUpdateHeraldry_JSON(t *testing.T) {
	post := func(f *fakeDaemon, form url.Values) (*httptest.ResponseRecorder, actionResponse) {
		t.Helper()
		req := actionPost("/b/gd-act1/heraldry", form, true)
		req.Header.Set("X-Auth-Request-Email", "matt@example.com")
		w := httptest.NewRecorder()
		mkServer(t, f).ServeHTTP(w, req)
		return w, decodeAction(t, w)
	}
	const arms = "blue \U0001F331 . \U0001F525 . \U0001F4E6"

	f := actionsFake()
	if _, res := post(f, url.Values{"heraldry": {"  " + arms + " "}}); res != (actionResponse{OK: true, Bead: "gd-act1"}) {
		t.Fatalf("save: %+v", res)
	}
	if f.updateLastID != "gd-act1" || f.updateLastReq.Heraldry == nil || *f.updateLastReq.Heraldry != arms {
		t.Errorf("sent %q to %s, want the trimmed arms", deref(f.updateLastReq.Heraldry), f.updateLastID)
	}

	f = actionsFake()
	if _, res := post(f, url.Values{"heraldry": {"none"}}); !res.OK || deref(f.updateLastReq.Heraldry) != "none" {
		t.Errorf("remove: %+v sent %q", res, deref(f.updateLastReq.Heraldry))
	}

	for _, c := range []struct {
		val  string
		code int
	}{{"", http.StatusBadRequest}, {"   ", http.StatusBadRequest}, {strings.Repeat("x", maxHeraldryForm+1), http.StatusRequestEntityTooLarge}} {
		f = actionsFake()
		w, res := post(f, url.Values{"heraldry": {c.val}})
		if w.Code != c.code || res.OK || res.Error == "" || f.updateCalls != 0 {
			t.Errorf("%.10q: HTTP %d %+v, %d writes; want %d and no write", c.val, w.Code, res, f.updateCalls, c.code)
		}
	}

	f = actionsFake()
	f.updateErr = &beadsapi.APIError{StatusCode: http.StatusConflict, Message: "the arms are already borne by gd-other (A plan) in project gardenio"}
	w, res := post(f, url.Values{"heraldry": {arms}})
	if w.Code != http.StatusConflict || res.OK || !strings.Contains(res.Error, "already borne by gd-other") {
		t.Errorf("a clash comes back in the daemon's words: HTTP %d %+v", w.Code, res)
	}
}

func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}
