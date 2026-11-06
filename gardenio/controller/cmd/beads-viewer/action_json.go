package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"mime"
	"net/http"
	"strings"
)

// Workbench JSON actions (spec gardenio/docs/superpowers/specs/2026-10-19-lands-workbench-glass-design.md
// §2.5.3, task gd-jgVYg6kH5K). The in-garden panels post to the viewer's own
// write routes (W-D2), so permission, audit and attribution stay those routes'.
// A caller that asks for JSON (Accept: application/json, or ?format=json — see
// wantsJSON) gets
//
//	{"ok":true,"bead":"<id>"}
//	{"ok":false,"error":"Decision is already resolved.","status":409}
//
// and every other caller (htmx, a plain form post) gets today's fragment,
// redirect or plain-text error byte for byte.
//
// Two pieces carry it. respondAction is called where a handler's message lives
// only inside an HTML fragment (the inline error banners), so a JSON caller gets
// that message rather than a status line. jsonAction wraps each write route and
// translates whatever else the handler (or a gate in front of it, such as
// operatorOnly) answered: a 2xx/3xx is ok, an http.Error's plain-text body is
// the error.

// actionResponse is the JSON answer of a write route.
type actionResponse struct {
	OK     bool   `json:"ok"`
	Bead   string `json:"bead,omitempty"`
	Error  string `json:"error,omitempty"`
	Status int    `json:"status,omitempty"`
	// Redirect is where the HTML answer would have sent the browser (an
	// HX-Redirect or a 3xx Location), e.g. the scrub-request bead a
	// request-scrub created.
	Redirect string `json:"redirect,omitempty"`
	// Agent is the name of the agent a quick-spawn started (Bead is its bead).
	Agent string `json:"agent,omitempty"`
}

// actionError is a write route's failure as the HTML fragment would show it:
// the inline message and the status it answers with.
type actionError struct {
	Msg    string
	Status int
}

func (e *actionError) Error() string { return e.Msg }

// actionFailure is the error a fragment renderer's (errMsg, status) pair stands
// for: nil when the render is a success (no message, 2xx), otherwise an
// actionError. A message rendered at 200 (htmx 2 swaps only a 2xx, so some
// forms answer their inline errors at 200) is still a failure; JSON calls it 400.
func actionFailure(msg string, status int) error {
	if msg == "" && status < http.StatusBadRequest {
		return nil
	}
	if status < http.StatusBadRequest {
		status = http.StatusBadRequest
	}
	if msg == "" {
		msg = http.StatusText(status)
	}
	return &actionError{Msg: msg, Status: status}
}

// wantsActionJSON reports whether a write route should answer JSON: a POST
// whose caller asked for it. GETs that share a renderer with a POST keep their
// HTML whatever they send.
func wantsActionJSON(r *http.Request) bool {
	return r.Method == http.MethodPost && wantsJSON(r)
}

// respondAction answers a JSON caller with the outcome of the action on bead
// and reports true; for any other caller it writes nothing and reports false,
// and the handler goes on to write its HTML. err is nil on success; an
// *actionError carries its own message and status, any other error answers
// through daemonWriteFailure's mapping.
func respondAction(w http.ResponseWriter, r *http.Request, bead string, err error) bool {
	if !wantsActionJSON(r) {
		return false
	}
	res := actionResponse{OK: err == nil, Bead: bead}
	if err == nil {
		res.Redirect = w.Header().Get("HX-Redirect")
	} else {
		var ae *actionError
		if !errors.As(err, &ae) {
			msg, status := daemonWriteFailure(err, "That did not go through.")
			ae = &actionError{Msg: msg, Status: status}
		}
		res.Error, res.Status = ae.Msg, ae.Status
		if res.Status < http.StatusBadRequest {
			res.Status = http.StatusBadRequest
		}
	}
	writeActionJSON(w, res)
	return true
}

func writeActionJSON(w http.ResponseWriter, res actionResponse) {
	code := http.StatusOK
	if !res.OK {
		code = res.Status
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(res)
}

// jsonAction gives a write route its JSON answer. A caller that did not ask for
// JSON goes straight through to next. For one that did, next writes into a
// buffer: an answer respondAction already made JSON passes through as it is;
// anything else is translated — a 2xx/3xx is {"ok":true}, a 4xx/5xx is
// {"ok":false} with an http.Error's plain-text message (or the status text
// when the body was not plain text).
func jsonAction(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !wantsActionJSON(r) {
			next.ServeHTTP(w, r)
			return
		}
		rec := &actionRecorder{header: http.Header{}}
		next.ServeHTTP(rec, r)
		code := rec.code
		if code == 0 {
			code = http.StatusOK
		}
		ct, _, _ := mime.ParseMediaType(rec.header.Get("Content-Type"))
		if ct == "application/json" {
			for k, v := range rec.header {
				w.Header()[k] = v
			}
			w.WriteHeader(code)
			_, _ = w.Write(rec.body.Bytes())
			return
		}
		for k, v := range rec.header {
			switch k {
			case "Content-Type", "Content-Length", "Location", "X-Content-Type-Options":
			default:
				w.Header()[k] = v
			}
		}
		res := actionResponse{OK: code < http.StatusBadRequest, Bead: r.PathValue("id")}
		if res.OK {
			res.Redirect = rec.header.Get("HX-Redirect")
			if res.Redirect == "" && code >= http.StatusMultipleChoices {
				res.Redirect = rec.header.Get("Location")
			}
		} else {
			res.Status = code
			if ct == "text/plain" {
				res.Error = strings.TrimSpace(rec.body.String())
			}
			if res.Error == "" {
				res.Error = http.StatusText(code)
			}
		}
		writeActionJSON(w, res)
	})
}

// actionRecorder buffers a write route's answer for jsonAction.
type actionRecorder struct {
	header http.Header
	code   int
	body   bytes.Buffer
}

func (a *actionRecorder) Header() http.Header { return a.header }

func (a *actionRecorder) WriteHeader(code int) {
	if a.code == 0 {
		a.code = code
	}
}

func (a *actionRecorder) Write(b []byte) (int, error) {
	if a.code == 0 {
		a.code = http.StatusOK
	}
	return a.body.Write(b)
}
